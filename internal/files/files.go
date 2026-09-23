// Package files handles bounded input and private, non-overwriting artifacts.
package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func ReadJSON(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	limited := &io.LimitedReader{R: f, N: 8<<20 + 1}
	d := json.NewDecoder(limited)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON value")
	}
	if limited.N <= 0 {
		return errors.New("JSON exceeds 8 MiB")
	}
	return nil
}

// WriteJSON publishes a complete file atomically and refuses to replace an
// existing artifact. New output names keep deployment evidence immutable.
func WriteJSON(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dashnet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		return fmt.Errorf("publish artifact (choose a new output path if it exists): %w", err)
	}
	return nil
}
