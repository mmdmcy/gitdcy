package main

// The personal words in binary files. git shows a binary file's change as
// "Binary files ... differ", with no lines to read, yet a build file (a
// .pyc, .pdb or .dll) can carry the owner's name in a home folder path,
// as UTF-8 or as Windows' UTF-16LE. So each binary file a pushed commit
// adds or changes has its new content read whole, as a stream, for each
// personal word in either encoding, in any case of its ASCII letters. No
// size is skipped: a blob git cannot give refuses the read.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
)

// binaryChunk is how much of a blob is held at a time.
const binaryChunk = 1 << 20

// binaryBlob is one binary file in a commit: the commit's place in the
// list being read, the file's path and its new blob.
type binaryBlob struct {
	commit int
	path   string
	id     string
}

// binaryDiffPath is the path in a diff header's "a/PATH b/PATH" (renames
// are off, so both are the same); the text as git wrote it otherwise (a
// quoted path), which is only shown.
func binaryDiffPath(names string) string {
	if len(names)%2 == 1 {
		half := len(names) / 2
		a, b := names[:half], names[half+1:]
		if strings.HasPrefix(a, "a/") && strings.HasPrefix(b, "b/") && a[2:] == b[2:] {
			return a[2:]
		}
	}
	return names
}

// binaryFold lowers ASCII letters in place. Folding the blob and the
// pattern alike makes a UTF-16LE word match in any case too.
func binaryFold(b []byte) []byte {
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return b
}

// binaryPatterns is each word as folded UTF-8 and folded UTF-16LE bytes;
// nil for an empty word.
func binaryPatterns(words []string) [][2][]byte {
	out := make([][2][]byte, len(words))
	for i, word := range words {
		word = rustLower(trim(word))
		if word == "" {
			continue
		}
		units := utf16.Encode([]rune(word))
		wide := make([]byte, 2*len(units))
		for j, u := range units {
			binary.LittleEndian.PutUint16(wide[2*j:], u)
		}
		out[i] = [2][]byte{binaryFold([]byte(word)), binaryFold(wide)}
	}
	return out
}

// binaryFind is the indexes of the words whose patterns r holds, read in
// chunks that overlap by the longest pattern less one byte.
func binaryFind(r io.Reader, patterns [][2][]byte) ([]int, error) {
	longest := 0
	for _, p := range patterns {
		longest = max(longest, len(p[1]))
	}
	found := make([]bool, len(patterns))
	buf := make([]byte, 0, binaryChunk+longest)
	chunk := make([]byte, binaryChunk)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, binaryFold(chunk[:n])...)
			for i, p := range patterns {
				if !found[i] && p[0] != nil && (bytes.Contains(buf, p[0]) || bytes.Contains(buf, p[1])) {
					found[i] = true
				}
			}
			if keep := longest - 1; keep > 0 && len(buf) > keep {
				buf = append(buf[:0], buf[len(buf)-keep:]...)
			} else if keep <= 0 {
				buf = buf[:0]
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	var out []int
	for i, f := range found {
		if f {
			out = append(out, i)
		}
	}
	return out, nil
}

// binaryCounter counts what is read through it.
type binaryCounter struct {
	r io.Reader
	n int64
}

func (c *binaryCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// binaryWords reads each blob's content (one `git cat-file --batch`, every
// blob streamed) for words; per blob, the indexes of the words it holds.
// A blob read twice (the same file in two commits) is read once.
func binaryWords(repo string, blobs []binaryBlob, words []string) ([][]int, error) {
	patterns := binaryPatterns(words)
	var ids []string
	seen := map[string]bool{}
	for _, b := range blobs {
		if !seen[b.id] {
			seen[b.id] = true
			ids = append(ids, b.id)
		}
	}
	cmd := gitCommand(repo, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, wrap(err, "read binary files")
	}
	if err := cmd.Start(); err != nil {
		return nil, wrap(err, "run git cat-file")
	}
	byID := map[string][]int{}
	readErr := func() error {
		in := bufio.NewReaderSize(pipe, binaryChunk)
		for _, id := range ids {
			header, err := in.ReadString('\n')
			if err != nil {
				return errf("git cat-file stopped before blob %s: %v", id, err)
			}
			fields := strings.Fields(header)
			if len(fields) != 3 {
				return errf("binary file blob %s could not be read (git cat-file: %s), so it was not checked for the personal words", id, trim(header))
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return errf("git cat-file gave no size for blob %s: %s", id, trim(header))
			}
			body := &binaryCounter{r: io.LimitReader(in, size)}
			found, err := binaryFind(body, patterns)
			if err != nil {
				return wrapf(err, "reading blob %s", id)
			}
			if body.n != size {
				return errf("git cat-file gave %d of blob %s's %d bytes", body.n, id, size)
			}
			if _, err := in.Discard(1); err != nil { // the newline after it
				return errf("git cat-file stopped inside blob %s", id)
			}
			byID[id] = found
		}
		return nil
	}()
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, readErr
	}
	if err := cmd.Wait(); err != nil {
		return nil, errf("git cat-file failed: %s", trim(stderr.String()))
	}
	out := make([][]int, len(blobs))
	for i, b := range blobs {
		out[i] = byID[b.id]
	}
	return out, nil
}
