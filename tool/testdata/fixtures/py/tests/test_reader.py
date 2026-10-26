from parser.reader import Normalize


def test_normalize():
    assert Normalize(" file ") == "file"
