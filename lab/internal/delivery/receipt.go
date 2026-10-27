package delivery

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// maxReceiptBytes bounds a receipt read; a receipt carries its reviewed diff.
const maxReceiptBytes = 8_000_000

// ReceiptStatus is the part of a delivery receipt the Lab UI shows.
type ReceiptStatus struct {
	State      string
	Lifecycle  string
	URL        string
	MoleculeID string
	// Commit is the commit Forgecell published, or "" before publication.
	Commit string
}

// ReadReceiptStatus reads deliveries/<id>.json for display without locking,
// writing or reconciling it. found is false when no receipt exists. It refuses a
// symlinked deliveries directory, a symlink, a non-regular or oversized file, a
// receipt for another Molecule and a state other than prepared, committed or
// published. The receipt state is reported exactly; publication is never
// inferred. Only the state, lifecycle state, URL, commit and Molecule id are kept: the
// JSON is streamed token by token, so the reviewed diff and every other field
// are validated as JSON and discarded rather than decoded.
func ReadReceiptStatus(lab, id string) (s ReceiptStatus, found bool, err error) {
	f, found, err := openReceipt(lab, id)
	if f == nil {
		return s, found, err
	}
	defer f.Close()
	limited := &io.LimitedReader{R: f, N: maxReceiptBytes + 1}
	s, err = decodeStatus(json.NewDecoder(limited))
	if limited.N <= 0 {
		return ReceiptStatus{}, true, fmt.Errorf("delivery receipt grew beyond %d bytes", maxReceiptBytes)
	}
	if err != nil {
		return ReceiptStatus{}, true, fmt.Errorf("malformed delivery receipt: %w", err)
	}
	if err := checkReceipt(id, s.MoleculeID, s.State); err != nil {
		return ReceiptStatus{}, true, err
	}
	return s, true, nil
}

// decodeStatus walks one top-level object, keeping state, url, commit, lifecycle.state
// and preview.moleculeId, and requires nothing but whitespace after it.
func decodeStatus(dec *json.Decoder) (ReceiptStatus, error) {
	var s ReceiptStatus
	err := object(dec, func(key string) error {
		switch key {
		case "state":
			return dec.Decode(&s.State)
		case "url":
			return dec.Decode(&s.URL)
		case "commit":
			return dec.Decode(&s.Commit)
		case "lifecycle":
			return nullableObject(dec, func(key string) error {
				if key == "state" {
					return dec.Decode(&s.Lifecycle)
				}
				return skip(dec)
			})
		case "preview":
			return object(dec, func(key string) error {
				if key == "moleculeId" {
					return dec.Decode(&s.MoleculeID)
				}
				return skip(dec)
			})
		}
		return skip(dec)
	})
	if err != nil {
		return ReceiptStatus{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ReceiptStatus{}, fmt.Errorf("data after the receipt object")
	}
	return s, nil
}

// object reads one JSON object, calling field for each key with the decoder
// positioned at its value; field must consume exactly that value.
func object(dec *json.Decoder, field func(string) error) error {
	if t, err := dec.Token(); err != nil {
		return err
	} else if t != json.Delim('{') {
		return fmt.Errorf("expected an object")
	}
	return members(dec, field)
}

// nullableObject is object, also accepting null as an absent object.
func nullableObject(dec *json.Decoder, field func(string) error) error {
	t, err := dec.Token()
	switch {
	case err != nil:
		return err
	case t == nil:
		return nil
	case t != json.Delim('{'):
		return fmt.Errorf("expected an object")
	}
	return members(dec, field)
}

func members(dec *json.Decoder, field func(string) error) error {
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("expected an object key")
		}
		if err := field(key); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// skip consumes one JSON value token by token without keeping it.
func skip(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

// openReceipt opens deliveries/<id>.json for reading. A nil file with found
// false and no error means no receipt exists.
func openReceipt(lab, id string) (*os.File, bool, error) {
	if !safeID.MatchString(id) {
		return nil, false, fmt.Errorf("invalid Molecule id")
	}
	dir := filepath.Join(lab, "deliveries")
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("deliveries directory is not a regular directory")
	}
	file := filepath.Join(dir, id+".json")
	info, err = os.Lstat(file)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxReceiptBytes {
		return nil, true, fmt.Errorf("delivery receipt is not a bounded regular file")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, true, err
	}
	if stat, err := f.Stat(); err != nil || !os.SameFile(info, stat) {
		f.Close()
		return nil, true, fmt.Errorf("opened delivery receipt identity mismatch")
	}
	return f, true, nil
}

func checkReceipt(id, moleculeID, state string) error {
	if moleculeID != id {
		return fmt.Errorf("delivery receipt identity mismatch")
	}
	if state != "prepared" && state != "committed" && state != "published" {
		return fmt.Errorf("invalid delivery state")
	}
	return nil
}
