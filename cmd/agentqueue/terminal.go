package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

// isTTY reports whether r is an interactive terminal. A character device is
// the useful portable check without an ioctl; /dev/null is excluded because
// services and hooks commonly receive it as stdin.
func isTTY(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if devNull, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, devNull) {
		return false
	}
	return true
}

func readLine(stdin io.Reader) (string, error) {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read answer: %w", err)
	}
	return line, nil
}
