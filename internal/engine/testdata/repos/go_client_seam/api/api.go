package api

import (
	"io"
	"net/http"
)

const base = "https://tasks.example.com"

func do(method, path string) ([]byte, error) {
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func Get(path string) ([]byte, error) { return do("GET", path) }
