import copy
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
from kanade_soulx import run
from kanade_soulx.validation import request, metadata
from kanade_soulx.worker import execute, validate_prompt_audio


class FakeJob:
    def __init__(self, params, output):
        self.params, self.output_dir = params, output
        self.artifacts, self.messages = [], []
    def check_cancelled(self):
        pass
    def progress(self, fraction, message):
        self.messages.append(message)
    def artifact(self, path, kind):
        self.artifacts.append((Path(path).name, kind))


class AdapterTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source, self.model = self.root/'upstream', self.root/'model'
        for p in ('cli/inference.py', 'soulxsinger/config/soulxsinger.yaml', 'soulxsinger/utils/phoneme/phone_set.json'):
            f = self.source/p
            f.parent.mkdir(parents=True, exist_ok=True)
            f.write_text('[]' if p.endswith('.json') else '')
        (self.source/'soulxsinger/utils/phoneme/phone_set.json').write_text(json.dumps(['zh_wo3','zh_ni3','<SP>','en_HH','en_AY']))
        self.model.mkdir()
        (self.model/'model.pt').touch()
        self.audio = self.root/'prompt.wav'
        self.audio.touch()
        self.row = dict(time=[0,1000], phoneme='zh_wo3',duration='1.0',note_pitch='60',note_type='2',text='我')
        self.prompt, self.target = self.root/'prompt.json', self.root/'target.json'
        self.prompt.write_text(json.dumps([self.row]))
        target = dict(self.row, phoneme='zh_ni3',text='你')
        self.target.write_text(json.dumps([target]))
        self.params = dict(python_path=sys.executable, source_dir=str(self.source), model_dir=str(self.model),
            prompt_audio=str(self.audio),prompt_metadata=str(self.prompt),target_metadata=str(self.target),device='cpu')
        self.output = self.root/'output'

    def test_default_score_and_correct_new_phonemes(self):
        p = request(self.params, self.output)
        self.assertEqual(p['control'],'score')
        self.assertEqual(p['target_metadata_data'][0]['phoneme'],'zh_ni3')
        self.assertEqual(p['pitch_shift'],0)
        self.assertEqual(Path(p['model_path']).name,'model.pt')

    def test_doctor_requires_no_metadata_audio(self):
        p = {k:self.params[k] for k in ('python_path','source_dir','model_dir')}
        self.assertTrue(request(p,self.output,doctor=True)['doctor'])

    def test_svc_checkpoint_not_accepted(self):
        (self.model/'model.pt').rename(self.model/'model-svc.pt')
        with self.assertRaises(ValueError): request(self.params,self.output)

    def test_remote_model_id_not_accepted(self):
        with self.assertRaises(ValueError): request(dict(self.params,model_dir='Soul-AILab/SoulX-Singer'),self.output)

    def test_melody_requires_f0(self):
        with self.assertRaisesRegex(ValueError,'F0'): request(dict(self.params,control='melody'),self.output)
        row=dict(self.row,f0=' '.join(['220']*50))
        self.prompt.write_text(json.dumps([row]));self.target.write_text(json.dumps([row]))
        self.assertEqual(request(dict(self.params,control='melody'),self.output)['control'],'melody')

    def test_prompt_time_must_start_zero(self):
        self.prompt.write_text(json.dumps([dict(self.row,time=[1000,2000])]))
        with self.assertRaises(ValueError):request(self.params,self.output)

    def test_prompt_single_segment(self):
        self.prompt.write_text(json.dumps([self.row,self.row]))
        with self.assertRaises(ValueError):request(self.params,self.output)

    def test_reject_malformed_metadata(self):
        for bad in [[],{},[dict(self.row,duration='NaN')],[dict(self.row,phoneme='zh_wo3 zh_ni3')],
                    [dict(self.row,time=[0,3000])],[dict(self.row,note_pitch='256')],
                    [dict(self.row,duration='0')],[dict(self.row,phoneme='unknown')],
                    [dict(self.row,f0='220')],[dict(self.row,f0='NaN '*50)],
                    [self.row,dict(self.row,time=[500,1500])]]:
            with self.subTest(bad=bad):
                self.target.write_text(json.dumps(bad))
                with self.assertRaises((ValueError,TypeError)):request(self.params,self.output)

    def test_unknown_control_device_and_pitch_rejected(self):
        for field,value in [('control','svc'),('device','auto'),('pitch_shift',False),('pitch_shift',25)]:
            with self.subTest(field=field), self.assertRaises(ValueError):
                request(dict(self.params,**{field:value}),self.output)

    def test_metadata_snapshot_not_changed_by_later_file_edit(self):
        p=request(self.params,self.output)
        self.target.write_text('[]')
        self.assertEqual(p['target_metadata_data'][0]['phoneme'],'zh_ni3')

    def test_success_transport_and_cleanup(self):
        job=FakeJob(self.params,self.output)
        def launch(args,**kwargs):
            p=json.loads(Path(args[2]).read_text())
            self.assertEqual(kwargs['cwd'],str(self.source))
            self.assertEqual(kwargs['env']['HF_HUB_OFFLINE'],'1')
            self.assertEqual(kwargs['stdin'],subprocess.DEVNULL)
            Path(p['result_path']).write_text('{}')
            (self.output/'vocal.wav').write_bytes(b'WAVE'*20)
            return SimpleNamespace(wait=lambda timeout:0,poll=lambda:0)
        with patch('kanade_soulx.subprocess.Popen',side_effect=launch):run(job)
        self.assertEqual(job.artifacts,[('vocal.wav','audio')])
        self.assertFalse(list(self.output.glob('soulx-*')))

    def test_failed_child_is_not_success(self):
        job=FakeJob(self.params,self.output)
        with patch('kanade_soulx.subprocess.Popen',return_value=SimpleNamespace(wait=lambda timeout:1,poll=lambda:1)):
            with self.assertRaises(RuntimeError):run(job)
        self.assertFalse(job.artifacts)

    def test_cancellation_terminates_child(self):
        job=FakeJob(self.params,self.output)
        job.check_cancelled=MagicMock(side_effect=Cancelled())
        proc=MagicMock();proc.poll.return_value=None
        with patch('kanade_soulx.subprocess.Popen',return_value=proc),self.assertRaises(Cancelled):run(job)
        proc.terminate.assert_called_once()
        self.assertFalse(job.artifacts)

    def test_worker_real_svs_contract(self):
        import numpy as np
        p=request(self.params,self.output)
        self.output.mkdir()
        config=SimpleNamespace(audio=SimpleNamespace(sample_rate=24000,hop_size=480),infer=SimpleNamespace(n_steps=32,cfg=3))
        generated=MagicMock();generated.squeeze.return_value=generated;generated.cpu.return_value=generated
        generated.numpy.return_value=np.ones(24000,dtype=np.float32)*.1
        model=SimpleNamespace(infer=MagicMock(return_value=generated))
        build=MagicMock(return_value=model)
        processor=MagicMock();processor.process.side_effect=lambda meta,wav:dict(meta=copy.deepcopy(meta),wav=wav)
        cls=MagicMock(return_value=processor)
        saved=[]
        def save(path,data,sr,subtype):
            saved.append((data,sr,subtype));Path(path).write_bytes(b'wav'*20)
        modules={'torch':SimpleNamespace(cuda=SimpleNamespace(is_available=lambda:False),no_grad=nullcontext,__version__='fake'),
            'soundfile':SimpleNamespace(info=lambda path:SimpleNamespace(channels=1,duration=1,samplerate=24000),read=lambda *a,**k:(np.zeros((24000,1)),24000),write=save),
            'cli.inference':SimpleNamespace(build_model=build),
            'soulxsinger.utils.file_utils':SimpleNamespace(load_config=lambda path:config),
            'soulxsinger.utils.data_processor':SimpleNamespace(DataProcessor=cls)}
        old_path=list(sys.path)
        try:
            with patch.dict(sys.modules,modules),patch('pathlib.Path.cwd',return_value=self.source):result=execute(p)
        finally:sys.path[:]=old_path
        build.assert_called_once_with(model_path=str(self.model/'model.pt'),config=config,device='cpu',use_fp16=False)
        call=model.infer.call_args
        self.assertEqual(call.kwargs,dict(auto_shift=False,pitch_shift=0,n_steps=32,cfg=3,control='score',use_fp16=False))
        self.assertEqual(call.args[0]['target']['meta']['phoneme'],'zh_ni3')
        self.assertEqual(result['sample_rate'],24000)
        self.assertEqual(saved[0][0].shape,(24000,))
        self.assertTrue((self.output/'vocal.wav').is_file())
        self.assertFalse((self.output/'vocal.tmp.wav').exists())

    def test_invalid_audio_rejected(self):
        import numpy as np
        for samples in (np.array([[float('nan')]]),np.array([[float('inf')]]),np.zeros((0,1))):
            sf=SimpleNamespace(info=lambda path:SimpleNamespace(channels=1,duration=1,samplerate=24000),read=lambda *a,**k:(samples,24000))
            with self.subTest(samples=samples),self.assertRaises(ValueError):validate_prompt_audio(sf,np,'input.wav',1)

    def test_worker_wrong_cwd_rejected(self):
        with self.assertRaises(ValueError):execute({'source_dir':'/not-current-dir'})

    def test_real_sdk_handshake_and_error(self):
        proc=subprocess.Popen([sys.executable,'-m','kanade_soulx'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
        try:
            hello=json.loads(proc.stdout.readline());self.assertIn('soulx.edit',hello['data']['tasks'])
            proc.stdin.write(json.dumps(dict(method='run',job='test',task='soulx.edit',params={},output_dir=str(self.output)))+'\n');proc.stdin.flush()
            response=json.loads(proc.stdout.readline());self.assertEqual(response['event'],'error')
            proc.stdin.write('{"method":"shutdown"}\n');proc.stdin.flush();proc.communicate(timeout=5)
            self.assertEqual(proc.returncode,0)
        finally:
            if proc.poll() is None:proc.kill();proc.communicate()


if __name__ == '__main__':unittest.main()
