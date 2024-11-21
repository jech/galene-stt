package wav

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
)

type Writer struct {
	w           io.WriteCloser
	wroteHeader bool
}

func Create(name string) (*Writer, error) {
	f, err := os.Create(name)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		w: f,
	}
	err = w.writeHeader()
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) writeHeader() error {
	const channels = 1
	const rate = 16000
	const bitsPerSample = 32
	const bytesPerBlock = channels * bitsPerSample / 8
	const bytesPerSec = rate * bytesPerBlock

	buf := &bytes.Buffer{}

	_, _ = buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
	_, _ = buf.WriteString("WAVE")

	_, _ = buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(3))
	_ = binary.Write(buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(rate))
	_ = binary.Write(buf, binary.LittleEndian, uint32(bytesPerSec))
	_ = binary.Write(buf, binary.LittleEndian, uint16(bytesPerBlock))
	_ = binary.Write(buf, binary.LittleEndian, uint16(bitsPerSample))

	_, _ = buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))

	_, err := w.w.Write(buf.Bytes())
	if err != nil {
		return err
	}
	w.wroteHeader = true
	return nil
}

func (w *Writer) Write(data []float32) error {
	return binary.Write(w.w, binary.LittleEndian, data)
}

func (w *Writer) Close() error {
	seeker, ok := w.w.(io.Seeker)
	if ok {
		size, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			w.w.Close()
			return err
		}
		_, err = seeker.Seek(4, io.SeekStart)
		if err != nil {
			w.w.Close()
			return err
		}
		err = binary.Write(
			w.w, binary.LittleEndian, uint32(size-8),
		)
		_, err = seeker.Seek(40, io.SeekStart)
		if err != nil {
			w.w.Close()
			return err
		}
		err = binary.Write(
			w.w, binary.LittleEndian, uint32(size-44),
		)
		_, _ = seeker.Seek(size, io.SeekStart)

	}
	return w.w.Close()
}
