import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from kanade_engine.runtime import Cancelled
from kanade_yingmusic import request, run


class Job:
    def __init__(self, p, output):
        self.params, self.output_dir = p, output
        self.artifacts, self.messages = [], []
        self.cancelled = False
    def progress(self, fraction, message):
        self.check_cancelled()
        self.messages.append(message)
    def artifact(self, path, kind):
        self.artifacts.append((Path(path).name, kind))
    def check_cancelled(self):
        if self.cancelled:
            raise Cancelled()


class AdapterTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.source = self.root / 'upstream'
        marker = self.source / 'src/YingMusicSinger/infer/YingMusicSinger.py'
        marker.parent.mkdir(parents=True)
        marker.touch()
        self.model = self.root / 'model'
        self.model.mkdir()
        (self.model / 'config.json').write_text('{}')
        (self.model / 'model.safetensors').touch()
        self.audio = self.root / 'vocal.wav'
        self.audio.touch()
        self.output = self.root / 'out'
        self.params = dict(python_path=sys.executable, source_dir=str(self.source), model_dir=str(self.model),
                           melody_audio=str(self.audio), ref_text='原词\n第二句', target_text='新词\n第二句')
    def test_empty_phrases_rejected(self):
        for value in ("a||b", "|a", "a|", "a\n\nb"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                request(dict(self.params, target_text=value), self.output)

    def test_default_reference_and_phrase_normalization(self):
        p = request(self.params, self.output)
        self.assertEqual(p['ref_audio'], str(self.audio))
        self.assertEqual(p['target_text'], '新词|第二句')
        self.assertEqual(p['seed'], -1)
        self.assertEqual(p['nfe_step'], 32)
    def test_reference_lyrics_required(self):
        for field in ('ref_text', 'target_text'):
            with self.subTest(field=field), self.assertRaises(ValueError):
                request(dict(self.params, **{field:'  '}), self.output)
    def test_missing_model_never_downloads(self):
        (self.model / 'model.safetensors').unlink()
        with self.assertRaises(ValueError):
            request(self.params, self.output)
    def test_remote_model_id_rejected(self):
        with self.assertRaises(ValueError):
            request(dict(self.params, model_dir='ASLP-lab/YingMusic-Singer'), self.output)
    def test_bad_numbers(self):
        for field, value in [('seed', True), ('seed', -2), ('nfe_step', 1.5), ('nfe_step', 129),
                             ('cfg_strength', float('nan')), ('cfg_strength', float('inf')), ('seed', '5')]:
            with self.subTest(field=field,value=value), self.assertRaises(ValueError):
                request(dict(self.params, **{field:value}), self.output)
    def test_missing_optional_reference_is_error(self):
        with self.assertRaises(ValueError):
            request(dict(self.params, ref_audio='/missing.wav'), self.output)
    def test_doctor_needs_no_audio_or_lyrics(self):
        p = {k:v for k,v in self.params.items() if k in ('python_path','source_dir','model_dir')}
        self.assertTrue(request(p, self.output, doctor=True)['doctor'])
    def fake_process(self, *, code=0, result=True, cancel=False):
        owner = self
        class Process:
            terminated = False
            def __init__(self, args, **kwargs):
                owner.assertEqual(args[0], str(Path(sys.executable).resolve()))
                owner.assertEqual(kwargs['cwd'], str(owner.source))
                owner.assertEqual(kwargs['env']['HF_HUB_OFFLINE'], '1')
                owner.assertNotIn('shell', kwargs)
                req = json.loads(Path(args[2]).read_text())
                owner.assertNotIn('新词', ' '.join(args))
                if result:
                    Path(req['result_path']).write_text(json.dumps({'seed':42,'message':'ok'}))
                    (owner.output/'vocal.wav').write_bytes(b'WAVE' * 20)
                self.running = cancel
            def wait(self, timeout=None):
                if self.running:
                    owner.job.cancelled = True
                    raise subprocess.TimeoutExpired('fake',timeout)
                return code
            def poll(self):
                return None if self.running else code
            def terminate(self):
                self.running = False
                self.terminated = True
        return Process
    def test_success_registers_audio_and_cleans_request(self):
        self.job = Job(self.params,self.output)
        with patch('kanade_yingmusic.subprocess.Popen',self.fake_process()):
            run(self.job)
        self.assertEqual(self.job.artifacts,[('vocal.wav','audio')])
        self.assertFalse(list(self.output.glob('yingmusic-*')))
    def test_failed_child_not_success(self):
        self.job = Job(self.params,self.output)
        with patch('kanade_yingmusic.subprocess.Popen',self.fake_process(code=1)), self.assertRaises(RuntimeError):
            run(self.job)
        self.assertEqual(self.job.artifacts,[])
    def test_missing_result_not_success(self):
        self.job = Job(self.params,self.output)
        with patch('kanade_yingmusic.subprocess.Popen',self.fake_process(result=False)), self.assertRaises(RuntimeError):
            run(self.job)
    def test_cancellation_terminates_child(self):
        self.job = Job(self.params,self.output)
        cls = self.fake_process(cancel=True)
        with patch('kanade_yingmusic.subprocess.Popen',cls), self.assertRaises(Cancelled):
            run(self.job)
        self.assertEqual(self.job.artifacts,[])
        self.assertFalse(list(self.output.glob('yingmusic-*')))


if __name__ == '__main__':
    unittest.main()
