package main

import (
	"io"
	"strings"
)

type ioReadCloser = io.ReadCloser

func strReader(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
