package snapshotstore

import (
	"context"
	"errors"
	"io"
)

var ErrHeadroom = errors.New("snapshot filesystem headroom exhausted")

// FilesystemAvailable is an observation, not a reservation, quota or guarantee
// that another process cannot fill this filesystem immediately afterwards.
func FilesystemAvailable(path string) (uint64, error) { return availableBytes(path) }

// ConfigureHeadroom runs before admission. Import streams recheck every 4 MiB;
// staging/reconstruction are checked at entry and before publication as well.
func (s *Store) ConfigureHeadroom(minimum uint64) error {
	if minimum > 1<<60 {
		return errors.New("invalid minimum filesystem headroom")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.active != 0 {
		return errors.New("imports are running")
	}
	s.minFree = minimum
	return s.checkHeadroom()
}
func (s *Store) checkHeadroom() error {
	if s.minFree == 0 {
		return nil
	}
	free, err := availableBytes(s.root)
	if err != nil {
		return errors.New("snapshot filesystem probe unavailable")
	}
	if free < s.minFree {
		return ErrHeadroom
	}
	return nil
}
func (s *Store) guardedProducer(produce Producer) Producer {
	return func(ctx context.Context, w io.Writer) error {
		if err := s.checkHeadroom(); err != nil {
			return err
		}
		return produce(ctx, &headroomWriter{writer: w, check: s.checkHeadroom})
	}
}

type headroomWriter struct {
	writer    io.Writer
	check     func() error
	remaining int64
}

func (w *headroomWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		if err := w.check(); err != nil {
			return 0, err
		}
		w.remaining = 4 << 20
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
