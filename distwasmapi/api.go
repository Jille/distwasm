package distwasmapi

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

var process func([]byte) ([]byte, error)

func Setup(f func([]byte) ([]byte, error)) {
	process = f
}

//go:export ProcessStdio
func ProcessStdio(size int) {
	data := make([]byte, size)
	if _, err := io.ReadFull(os.Stdin, data); err != nil {
		fmt.Fprintf(os.Stderr, "error reading input: %v", err)
		os.Exit(120)
	}

	ret, err := process(data)
	if err != nil {
		os.Stderr.Write([]byte(err.Error()))
		os.Exit(1)
	}
	if err := binary.Write(os.Stdout, binary.LittleEndian, uint32(len(ret))); err != nil {
		fmt.Fprintf(os.Stderr, "error writing result: %v", err)
		os.Exit(120)
	}
	if _, err := os.Stdout.Write(ret); err != nil {
		fmt.Fprintf(os.Stderr, "error writing result: %v", err)
		os.Exit(120)
	}
}
