package kubernetes

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeKubectl struct {
	binary   string
	argsLog  string
	manifest string
}

func newFakeKubectl(t *testing.T) fakeKubectl {
	t.Helper()
	dir := t.TempDir()
	fake := fakeKubectl{
		binary:   filepath.Join(dir, "kubectl"),
		argsLog:  filepath.Join(dir, "args.log"),
		manifest: filepath.Join(dir, "manifest.json"),
	}
	script := `#!/bin/sh
for arg do printf '%s\n' "$arg" >> "$KUBECTL_ARGS_LOG"; done
while [ "$#" -gt 0 ]; do
  case "$1" in
    --kubeconfig|-n) shift 2 ;;
    *) break ;;
  esac
done
command_name=$1
shift
case "$command_name" in
  apply) cat > "$KUBECTL_MANIFEST" ;;
  get) printf '%s' "${KUBECTL_POD_PHASE:-Running}" ;;
  delete) ;;
  exec)
    if [ "$1" = "-i" ]; then shift; fi
    shift
    if [ "$1" = "--" ]; then shift; fi
    shift
    shift
    payload=$1
    shift
    exec sh -c "$payload" "$@"
    ;;
  *) printf 'unexpected kubectl command: %s\n' "$command_name" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(fake.binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECTL_ARGS_LOG", fake.argsLog)
	t.Setenv("KUBECTL_MANIFEST", fake.manifest)
	return fake
}
