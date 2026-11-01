import unittest

import stats


class TestStats(unittest.TestCase):
    def test_mean(self):
        self.assertEqual(stats.mean([1, 2, 3, 4]), 2.5)

    def test_median_odd_sorted(self):
        self.assertEqual(stats.median([1, 2, 3]), 2)

    def test_range(self):
        self.assertEqual(stats.value_range([4, -1, 7]), 8)


if __name__ == "__main__":
    unittest.main()
