import unittest
try:
    import numpy as np
except ImportError:
    np = None
from kanade_vevo2.worker import mix_audio


@unittest.skipIf(np is None, 'optional NumPy needed for numerical mixing test')
class MixTests(unittest.TestCase):
    def test_tails_stereo_and_clipping(self):
        class Tensor(np.ndarray):
            def expand(self, channels, length):
                return np.broadcast_to(self,(channels,self.shape[1])).copy().view(Tensor)
            def abs(self):
                return np.abs(self)
        class Torch:
            @staticmethod
            def zeros(shape,dtype):return np.zeros(shape,dtype=dtype).view(Tensor)
        def tensor(values):return np.array(values,dtype=float).view(Tensor)
        mixed=mix_audio(tensor([[.2,.2]]),tensor([[.3,.3,.3,.3],[.1,.1,.1,.1]]),Torch)
        np.testing.assert_allclose(mixed,[[.5,.5,.3,.3],[.3,.3,.1,.1]])
        mixed=mix_audio(tensor([[.8,.8,.4,.4]]),tensor([[.8,.8]]),Torch)
        np.testing.assert_allclose(mixed,[[1,1,.25,.25]])
        with self.assertRaises(ValueError):mix_audio(tensor([[.1],[.2]]),tensor([[.1],[.2],[.3]]),Torch)
