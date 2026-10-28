package tenantexport

import (
	"bytes"
	"io"
	"os"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// spoolTemp copies the stream to a temp file so the archive can be
// walked twice (verify pass, then import pass) without holding it in
// memory.
func spoolTemp(r io.Reader) (string, error) {
	f, err := os.CreateTemp("", "oga9exp-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func openTemp(path string) (*os.File, error) { return os.Open(path) }

func removeTemp(path string) { _ = os.Remove(path) }
