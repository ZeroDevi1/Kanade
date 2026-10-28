"""rhythm 拍点规整的回归测试。

运行（用引擎的虚拟环境，PYTHONPATH 指向仓库源码）：
    python -m unittest discover -s engines/score/tests
"""

import unittest

import numpy as np

from kanade_score import rhythm


class RegularizeTest(unittest.TestCase):
    def test_fills_long_gaps_monotonically(self):
        # 《我不想念》的两处漏拍：开头 0.78 → 2.52 缺 3 拍，3:54 的长音处 234.24 → 238.48 缺 8 拍。
        # 补拍曾按上一个补拍累加，得到 3.39 > 2.52、251.2 > 238.48 的乱序拍点。
        grid = np.arange(0.78, 245.0, 0.46)
        missing = list(range(1, 4)) + list(range(500, 508))
        out = rhythm.regularize(np.delete(grid, missing))

        self.assertTrue(np.all(np.diff(out) > 0), "拍点须严格递增")
        np.testing.assert_allclose(out, grid, atol=1e-6)

    def test_merges_duplicates_and_keeps_regular_beats(self):
        ibi = 0.5
        grid = np.arange(0.0, 20.0, ibi)
        # 相隔 0.08 秒的重复拍合并为两者的中点。
        doubled = np.sort(np.append(grid, 10.08))
        out = rhythm.regularize(doubled)

        self.assertTrue(np.all(np.diff(out) > 0))
        self.assertEqual(len(out), len(grid))
        np.testing.assert_allclose(out[grid != 10.0], grid[grid != 10.0])
        self.assertAlmostEqual(float(out[grid == 10.0][0]), 10.04)
        np.testing.assert_allclose(rhythm.regularize(grid), grid)


if __name__ == "__main__":
    unittest.main()
