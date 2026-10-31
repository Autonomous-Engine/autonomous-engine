from calc.fmt import show_product


def test_flow(base):
    assert show_product(base, 2) == "10 * 2 = 20"
