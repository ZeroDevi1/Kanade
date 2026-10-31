"""Exercise the actual worker API without installing ML packages or weights."""
from contextlib import nullcontext
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

from kanade_yingmusic.worker import execute


class WorkerTests(unittest.TestCase):
    def test_local_model_api_and_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config_path = root/'model.yaml'
            config_path.touch()
            (root/'config.json').write_text(json.dumps({'model_cfg_path':str(config_path),'vae_config_path':str(config_path)}))
            p = dict(source_dir=str(root),model_dir=str(root),output_dir=str(root),melody_audio='melody.wav',
                     ref_audio='timbre.wav',ref_text='reference words',target_text='new words',nfe_step=32,cfg_strength=3.,seed=7)
            audio = MagicMock()
            audio.ndim = 2
            audio.shape = (2,44100)
            audio.to.return_value = audio
            finite = SimpleNamespace(all=lambda: True)
            torch = SimpleNamespace(cuda=SimpleNamespace(is_available=lambda:False),isfinite=lambda x:finite,
                                    inference_mode=nullcontext,__version__='fake')
            torchaudio = SimpleNamespace(load=lambda path:(audio,44100),
                                         save=lambda path,wav,sample_rate:Path(path).write_bytes(b'wav'))
            model = MagicMock(return_value=(audio,44100))
            model.to.return_value = model
            cls = MagicMock()
            cls.from_pretrained.return_value = model
            mods={'torch':torch,'torchaudio':torchaudio,
                  'src.YingMusicSinger.infer.YingMusicSinger':SimpleNamespace(YingMusicSinger=cls)}
            old_path=list(sys.path)
            try:
                with patch.dict(sys.modules,mods),patch('pathlib.Path.cwd',return_value=root):
                    result=execute(p)
            finally:
                sys.path[:]=old_path
            cls.from_pretrained.assert_called_once_with(str(root),local_files_only=True)
            model.assert_called_once_with(ref_audio_path='timbre.wav',melody_audio_path='melody.wav',
                ref_text='reference words',target_text='new words',lrc_align_mode='sentence_level',
                sil_len_to_end=.5,t_shift=.5,nfe_step=32,cfg_strength=3.,seed=7)
            self.assertEqual(result,{'seed':7,'sample_rate':44100})
            self.assertTrue((root/'vocal.wav').is_file())
            self.assertFalse((root/'vocal.wav.tmp.wav').exists())
    def test_wrong_working_directory_fails_before_import(self):
        with self.assertRaises(ValueError):
            execute({'source_dir':'/not-the-current-working-directory'})

class ProtocolTests(unittest.TestCase):
    def test_real_sdk_handshake_and_validation_error(self):
        import os
        import subprocess
        with tempfile.TemporaryDirectory() as directory:
            proc = subprocess.Popen([sys.executable,'-m','kanade_yingmusic'], stdin=subprocess.PIPE,
                                    stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,env=dict(os.environ))
            try:
                hello=json.loads(proc.stdout.readline())
                self.assertEqual(hello['event'],'hello')
                self.assertIn('yingmusic.edit',hello['data']['tasks'])
                proc.stdin.write(json.dumps({'method':'run','job':'test','task':'yingmusic.edit',
                                             'params':{},'output_dir':directory})+'\n')
                proc.stdin.flush()
                response=json.loads(proc.stdout.readline())
                self.assertEqual(response['event'],'error')
                self.assertEqual(response['job'],'test')
                self.assertIn('YingMusic Python',response['data']['message'])
                proc.stdin.write('{"method":"shutdown"}\n')
                proc.stdin.flush()
                proc.communicate(timeout=5)
                self.assertEqual(proc.returncode,0)
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.communicate()


if __name__ == '__main__':
    unittest.main()
