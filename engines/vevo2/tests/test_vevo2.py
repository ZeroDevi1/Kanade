from contextlib import nullcontext
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

from kanade_engine.runtime import Cancelled
from kanade_vevo2 import AR, FM, REQUIRED, request, run
from kanade_vevo2.worker import execute, local_loaders, pipeline_paths


class Fixture(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.source, self.model, self.output = self.root/'source', self.root/'model', self.root/'output'
        for path in [self.source/'models/svc/vevo2/vevo2_utils.py', self.root/'medium.pt', self.root/'audio.wav',
                     self.model/FM/'whisper_stats.pt'] + [self.model/name for name in REQUIRED]:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b'fixture')
        self.p = dict(python_path=sys.executable,source_dir=str(self.source),model_dir=str(self.model),
                      whisper_path=str(self.root/'medium.pt'),source_audio=str(self.root/'audio.wav'),
                      original_text='old lyrics',target_text='new lyrics',license_accepted=True)

    def test_valid_defaults(self):
        p = request(self.p,self.output)
        self.assertEqual(p['flow_matching_steps'],32)
        self.assertEqual(p['whisper_stats_path'],str(self.model/FM/'whisper_stats.pt'))

    def test_doctor_needs_no_audio(self):
        p=dict(self.p);del p['source_audio'];del p['original_text'];del p['target_text']
        self.assertTrue(request(p,self.output,doctor=True)['doctor'])

    def test_license_required_even_doctor(self):
        for accepted in (False,'true',None):
            for doctor in (True,False):
                with self.assertRaises(ValueError):request(dict(self.p,license_accepted=accepted),self.output,doctor=doctor)

    def test_required_original_words_and_local_medium(self):
        for update in ({'original_text':''},{'target_text':''},{'whisper_path':'medium'}, {'model_dir':'RMSnow/Vevo2'}, {'source_audio':'missing'}):
            with self.subTest(update=update),self.assertRaises(ValueError):request(dict(self.p,**update),self.output)

    def test_incomplete_model_fails_without_spawn(self):
        (self.model/AR/'model.safetensors').unlink()
        with patch('kanade_vevo2.subprocess.Popen') as spawn,self.assertRaises(ValueError):request(self.p,self.output)
        spawn.assert_not_called()

    def test_numeric_ranges(self):
        for key,value in [('seed',True),('seed',-1),('flow_matching_steps',0),('flow_matching_steps',float('nan'))]:
            with self.assertRaises(ValueError):request(dict(self.p,**{key:value}),self.output)

    def job(self):
        job=SimpleNamespace(params=self.p,output_dir=self.output,progress=MagicMock(),artifact=MagicMock(),check_cancelled=MagicMock())
        return job

    def test_cancellation_kills_child(self):
        proc=MagicMock();proc.poll.return_value=None;proc.wait.side_effect=[subprocess.TimeoutExpired('worker',.15),0]
        job=self.job();job.check_cancelled.side_effect=Cancelled()
        with patch('kanade_vevo2.subprocess.Popen',return_value=proc),self.assertRaises(Cancelled):run(job)
        proc.terminate.assert_called_once();job.artifact.assert_not_called()
        self.assertFalse(list(self.output.glob('vevo2-*')))

    def test_failure_never_registers_outputs(self):
        proc=MagicMock();proc.wait.return_value=1;proc.poll.return_value=1
        job=self.job()
        with patch('kanade_vevo2.subprocess.Popen',return_value=proc),self.assertRaises(RuntimeError):run(job)
        job.artifact.assert_not_called()

    def test_success_and_private_request_cleanup(self):
        job=self.job()
        def spawn(args,**kwargs):
            p=json.loads(Path(args[-1]).read_text());self.assertEqual(kwargs['cwd'],str(self.source))
            self.assertEqual(kwargs['env']['HF_HUB_OFFLINE'],'1');self.assertNotIn('new lyrics',' '.join(args))
            Path(p['result_path']).write_text('{"sample_rate":24000,"seed":0}')
            (self.output/'vocal.wav').write_bytes(b'0'*100)
            proc=MagicMock();proc.wait.return_value=0;proc.poll.return_value=0;return proc
        with patch('kanade_vevo2.subprocess.Popen',side_effect=spawn):run(job)
        job.artifact.assert_called_once_with(self.output/'vocal.wav','audio')
        self.assertFalse(list(self.output.glob('vevo2-*')))

    def test_loader_redirects_are_local_and_restored(self):
        cfg=SimpleNamespace(model=SimpleNamespace(flow_matching_transformer=SimpleNamespace(),coco=SimpleNamespace()))
        old_config=MagicMock(return_value=cfg);old_ar=MagicMock();old_whisper=MagicMock()
        model=MagicMock();auto=SimpleNamespace(from_pretrained=MagicMock(return_value=model))
        upstream=SimpleNamespace(build_ar_model=old_ar,load_config=old_config,AutoModelForCausalLM=auto)
        whisper=SimpleNamespace(load_model=old_whisper);p=request(self.p,self.output)
        with local_loaders(upstream,whisper,p):
            whisper.load_model('medium','cpu')
            old_whisper.assert_called_once_with(str(self.root/'medium.pt'),'cpu')
            with self.assertRaises(ValueError):whisper.load_model('large','cpu')
            upstream.build_ar_model(str(self.model/AR),'cpu')
            auto.from_pretrained.assert_called_once_with(str(self.model/AR),device_map='cpu',torch_dtype='auto',local_files_only=True,trust_remote_code=False)
            upstream.load_config(str(self.model/FM/'config.json'))
            self.assertEqual(cfg.model.coco.whisper_stats_path,p['whisper_stats_path'])
            cfg.model.flow_matching_transformer.use_text_as_condition=True
            with self.assertRaises(ValueError):upstream.load_config(str(self.model/FM/'config.json'))
        self.assertIs(whisper.load_model,old_whisper);self.assertIs(upstream.build_ar_model,old_ar);self.assertIs(upstream.load_config,old_config)

    def test_actual_ar_fm_editing_contract(self):
        self.output.mkdir();p=request(self.p,self.output)
        cfg=SimpleNamespace(model=SimpleNamespace(flow_matching_transformer=SimpleNamespace(),coco=SimpleNamespace()))
        audio=MagicMock();audio.ndim=2;audio.shape=(1,24000);audio.detach.return_value=audio;audio.to.return_value=audio;audio.abs.return_value.max.return_value=.7
        pipeline=SimpleNamespace(inference_ar_and_fm=MagicMock(return_value=audio))
        upstream=SimpleNamespace(load_config=MagicMock(return_value=cfg),build_ar_model=MagicMock(),AutoModelForCausalLM=MagicMock(),Vevo2InferencePipeline=MagicMock(return_value=pipeline))
        torch=SimpleNamespace(cuda=SimpleNamespace(is_available=lambda:False),device=lambda x:x,manual_seed=MagicMock(),inference_mode=nullcontext,isfinite=lambda x:SimpleNamespace(all=lambda:True))
        torchaudio=SimpleNamespace(load=lambda path:(audio,24000),save=lambda path,wav,sample_rate:Path(path).write_bytes(b'wave'))
        mods={'torch':torch,'torchaudio':torchaudio,'whisper':SimpleNamespace(load_model=MagicMock()),
              'models':SimpleNamespace(),'models.svc':SimpleNamespace(),'models.svc.vevo2':SimpleNamespace(vevo2_utils=upstream)}
        old_path=list(sys.path)
        try:
            with patch.dict(sys.modules,mods),patch('pathlib.Path.cwd',return_value=self.source):result=execute(p)
        finally:sys.path[:]=old_path
        upstream.Vevo2InferencePipeline.assert_called_once_with(**pipeline_paths(str(self.model)),device='cpu',use_vllm=False)
        pipeline.inference_ar_and_fm.assert_called_once_with(target_text='new lyrics',prosody_wav_path=p['source_audio'],style_ref_wav_path=p['source_audio'],style_ref_wav_text='old lyrics',timbre_ref_wav_path=p['source_audio'],use_prosody_code=True,use_pitch_shift=False,flow_matching_steps=32,display_audio=False)
        self.assertEqual(result,{'sample_rate':24000,'seed':0});self.assertTrue((self.output/'vocal.wav').is_file())

    def test_wrong_cwd_before_ml_imports(self):
        with self.assertRaises(ValueError):execute({'source_dir':str(self.source)})


class ProtocolTests(unittest.TestCase):
    def test_sdk_hello_and_validation_error(self):
        proc=subprocess.Popen([sys.executable,'-m','kanade_vevo2'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
        try:
            hello=json.loads(proc.stdout.readline());self.assertIn('vevo2.edit',hello['data']['tasks'])
            proc.stdin.write(json.dumps({'method':'run','job':'test','task':'vevo2.edit','params':{},'output_dir':'unused'})+'\n');proc.stdin.flush()
            response=json.loads(proc.stdout.readline());self.assertEqual(response['event'],'error');self.assertIn('CC-BY-NC-ND',response['data']['message'])
            proc.stdin.write('{"method":"shutdown"}\n');proc.stdin.flush();proc.communicate(timeout=5);self.assertEqual(proc.returncode,0)
        finally:
            if proc.poll() is None:proc.kill();proc.communicate()


if __name__=='__main__':unittest.main()
