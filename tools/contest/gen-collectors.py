"""Allow a runtime poll to survive the measured goroutine snapshot pause."""
from pathlib import Path

source = Path('tools/measurectl/collectors.yaml').read_text()
needle = '-endpoint {var:runtime_metrics_url}\n      -interval 1s'
assert source.count(needle) == 1, 'runtime collector definition changed'
source = source.replace(needle, needle + '\n      -timeout 3s')
output = Path('generated/collectors.yaml')
output.parent.mkdir(exist_ok=True)
output.write_text(source)
