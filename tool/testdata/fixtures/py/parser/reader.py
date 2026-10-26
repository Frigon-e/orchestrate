def Normalize(value):
    """Trim a file name without changing its case."""
    return value.strip()

class Parser:
    def Parse(self, value):
        return Normalize(value)
