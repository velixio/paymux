package notify

import (
	"encoding/base64"
	"io"
	"mime/quotedprintable"
)

func base64Reader(r io.Reader) io.Reader { return base64.NewDecoder(base64.StdEncoding, r) }

func qpReader(r io.Reader) io.Reader { return quotedprintable.NewReader(r) }
