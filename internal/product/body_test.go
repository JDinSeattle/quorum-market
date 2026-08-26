package product

import (
	"io"
	"strings"
)

func newBody(s string) io.Reader { return strings.NewReader(s) }
