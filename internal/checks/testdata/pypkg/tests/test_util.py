import importlib

# The module is loaded dynamically, so the import graph cannot see it; the
# test is selected for calc/util.py by its name.
util = importlib.import_module("calc.util")


def test_clamp():
    assert util.clamp(5, 0, 3) == 3
