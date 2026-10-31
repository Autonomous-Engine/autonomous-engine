from .ops import (
    add,
    mul,
)


def show_sum(a, b):
    return f"{a} + {b} = {add(a, b)}"


def show_product(a, b):
    return f"{a} * {b} = {mul(a, b)}"
