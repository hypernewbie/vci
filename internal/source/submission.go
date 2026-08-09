package source

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Submission is a client build request framed for transfer: the Git identity,
// the objects the coordinator lacks as a Git bundle, and the client's local
// changes. PackageSubmission and UnpackageSubmission are its wire framing.
type Submission struct {
	Head         string
	Base         string
	RemoteURL    string
	Have         string
	Bundle       []byte
	LocalChanges LocalChanges
}

// submissionMeta is the identity record carried inside the submission tar.
type submissionMeta struct {
	Head      string `json:"head"`
	Base      string `json:"base"`
	RemoteURL string `json:"remote_url"`
	Have      string `json:"have"`
}

// PackageSubmission serializes a submission into a deterministic tar: a
// meta.json entry, an optional bundle entry, and an lc.tar entry holding the
// serialized local changes. UnpackageSubmission is the inverse.
func PackageSubmission(s Submission) (io.ReadCloser, error) {
	lcRC, err := PackageLC(s.LocalChanges)
	if err != nil {
		return nil, err
	}
	lcBytes, err := io.ReadAll(lcRC)
	_ = lcRC.Close()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	meta, err := json.Marshal(submissionMeta{Head: s.Head, Base: s.Base, RemoteURL: s.RemoteURL, Have: s.Have})
	if err != nil {
		return nil, err
	}
	if err := writeRegEntry(tw, "meta.json", meta); err != nil {
		return nil, err
	}
	if len(s.Bundle) > 0 {
		if err := writeRegEntry(tw, "bundle", s.Bundle); err != nil {
			return nil, err
		}
	}
	if err := writeRegEntry(tw, "lc.tar", lcBytes); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

// UnpackageSubmission reads a submission tar and is safe on untrusted input:
// it reads entries into memory and never writes to disk.
func UnpackageSubmission(r io.Reader) (Submission, error) {
	tr := tar.NewReader(r)
	var s Submission
	var lcBytes []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Submission{}, fmt.Errorf("read submission tar: %w", err)
		}
		switch h.Name {
		case "meta.json":
			data, err := io.ReadAll(tr)
			if err != nil {
				return Submission{}, err
			}
			var m submissionMeta
			if err := json.Unmarshal(data, &m); err != nil {
				return Submission{}, fmt.Errorf("decode submission meta: %w", err)
			}
			s.Head, s.Base, s.RemoteURL, s.Have = m.Head, m.Base, m.RemoteURL, m.Have
		case "bundle":
			data, err := io.ReadAll(tr)
			if err != nil {
				return Submission{}, err
			}
			s.Bundle = data
		case "lc.tar":
			data, err := io.ReadAll(tr)
			if err != nil {
				return Submission{}, err
			}
			lcBytes = data
		default:
			return Submission{}, fmt.Errorf("unexpected submission tar entry %q", h.Name)
		}
	}
	if lcBytes != nil {
		lc, err := UnpackageLC(bytes.NewReader(lcBytes))
		if err != nil {
			return Submission{}, err
		}
		s.LocalChanges = lc
	}
	return s, nil
}

func writeRegEntry(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
