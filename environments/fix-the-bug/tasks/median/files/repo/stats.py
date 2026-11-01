"""Tiny statistics helpers."""


def mean(xs):
    """Arithmetic mean of a non-empty sequence."""
    if not xs:
        raise ValueError("mean of empty sequence")
    return sum(xs) / len(xs)


def median(xs):
    """Median of a non-empty sequence of numbers."""
    if not xs:
        raise ValueError("median of empty sequence")
    n = len(xs)
    return xs[n // 2]


def value_range(xs):
    """max(xs) - min(xs) of a non-empty sequence."""
    if not xs:
        raise ValueError("range of empty sequence")
    return max(xs) - min(xs)
