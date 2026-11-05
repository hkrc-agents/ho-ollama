//go:build harmonyos

package nativeprocess

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// FollowOutput streams newly appended SO bytes without interpreting Unicode,
// ANSI, or log events. done closes only after official exit proof; a final read
// then drains the remaining bytes. Output is bounded per invocation.
func (p *Invocation) FollowOutput(ctx context.Context, done <-chan struct{}, out io.Writer) error {
	var files [2]*os.File
	var offsets [2]int64
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	buffer := make([]byte, 32768)
	var total int64
	for {
		finished := false
		select {
		case <-done:
			finished = true
		default:
		}
		for n, name := range []string{"stdout", "stderr"} {
			if files[n] == nil {
				f, err := os.Open(filepath.Join(p.Directory, name))
				if err != nil {
					if os.IsNotExist(err) && !finished {
						continue
					}
					return err
				}
				files[n] = f
			}
			for {
				count, err := files[n].ReadAt(buffer, offsets[n])
				if count > 0 {
					total += int64(count)
					if total > 64*1024*1024 {
						return errors.New("managed invocation output budget exceeded")
					}
					written, e := out.Write(buffer[:count])
					if e != nil {
						return e
					}
					if written != count {
						return io.ErrShortWrite
					}
					offsets[n] += int64(count)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
			}
		}
		if finished {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		case <-time.After(25 * time.Millisecond):
		}
	}
}
