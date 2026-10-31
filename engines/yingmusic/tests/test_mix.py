"""Numerical mix tests; NumPy is optional in the thin adapter test environment."""
import unittest
from types import SimpleNamespace
from kanade_yingmusic.worker import mix_audio
try:
    import numpy as np
except ImportError:
    np = None


@unittest.skipIf(np is None, 'NumPy unavailable (not a runtime adapter dependency)')
class MixTests(unittest.TestCase):
    def setUp(self):
        class Tensor(np.ndarray):
            def abs(self):
                return np.abs(self)
            def expand(self, channels, _):
                return np.broadcast_to(self, (channels, self.shape[1])).view(Tensor)
        self.tensor=lambda values:np.array(values,dtype=float).view(Tensor)
        self.torch=SimpleNamespace(zeros=lambda shape,dtype:np.zeros(shape,dtype=dtype).view(Tensor))
    def test_preserves_longer_accompaniment_and_expands_mono(self):
        mixed=mix_audio(self.tensor([[.1,.2]]),self.tensor([[.3,.3,.3],[.2,.2,.2]]),self.torch)
        np.testing.assert_allclose(mixed,[[.4,.5,.3],[.3,.4,.2]])
    def test_preserves_longer_vocal_and_avoids_clipping(self):
        mixed=mix_audio(self.tensor([[.8,.8,.8]]),self.tensor([[.8,.8]]),self.torch)
        np.testing.assert_allclose(mixed,[[1,1,.5]])
    def test_rejects_incompatible_multichannel_audio(self):
        with self.assertRaises(ValueError):
            mix_audio(self.tensor([[0],[0]]),self.tensor([[0],[0],[0]]),self.torch)
