package backend

import (
	"strings"
)

// errAfterReader returns data on the first reads, then the given error.
type errAfterReader struct {
	r    *strings.Reader
	err  error
	init bool
}

func newErrAfterReader(data string, err error) *errAfterReader {
	return &errAfterReader{r: strings.NewReader(data), err: err}
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil {
		return n, r.err
	}
	return n, nil
}

// mustOpenAI creates an OpenAIBackend for testing; panics on error.
func mustOpenAI(name, baseURL, apiKey string) *OpenAIBackend {
	b, err := NewOpenAI(name, baseURL, apiKey)
	if err != nil {
		panic(err)
	}
	return b
}
