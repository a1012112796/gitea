// Copyright 2015 The Gogs Authors. All rights reserved.
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package highlight

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	gohtml "html"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"code.gitea.io/gitea/modules/analyze"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	lru "github.com/hashicorp/golang-lru"
)

// don't index files larger than this many bytes for performance purposes
const sizeLimit = 1024 * 1024

var (
	// For custom user mapping
	highlightMapping = map[string]string{}

	once sync.Once

	cache *lru.TwoQueueCache

	githubStyles = styles.Get("github")
)

// NewContext loads custom highlight map from local config
func NewContext() {
	once.Do(func() {
		highlightMapping = setting.GetHighlightMapping()

		// The size 512 is simply a conservative rule of thumb
		c, err := lru.New2Q(512)
		if err != nil {
			panic(fmt.Sprintf("failed to initialize LRU cache for highlighter: %s", err))
		}
		cache = c
	})
}

// Code returns a HTML version of code string with chroma syntax highlighting classes and the matched lexer name
func Code(fileName, language, code string) (string, string) {
	NewContext()

	// diff view newline will be passed as empty, change to literal '\n' so it can be copied
	// preserve literal newline in blame view
	if code == "" || code == "\n" {
		return "\n", ""
	}

	if len(code) > sizeLimit {
		return code, ""
	}

	var lexer chroma.Lexer

	if len(language) > 0 {
		lexer = lexers.Get(language)

		if lexer == nil {
			// Attempt stripping off the '?'
			if idx := strings.IndexByte(language, '?'); idx > 0 {
				lexer = lexers.Get(language[:idx])
			}
		}
	}

	if lexer == nil {
		if val, ok := highlightMapping[filepath.Ext(fileName)]; ok {
			// use mapped value to find lexer
			lexer = lexers.Get(val)
		}
	}

	if lexer == nil {
		if l, ok := cache.Get(fileName); ok {
			lexer = l.(chroma.Lexer)
		}
	}

	if lexer == nil {
		lexer = lexers.Match(fileName)
		if lexer == nil {
			lexer = lexers.Fallback
		}
		cache.Add(fileName, lexer)
	}

	lexerName := formatLexerName(lexer.Config().Name)

	return CodeFromLexer(lexer, code), lexerName
}

// CodeFromLexer returns a HTML version of code string with chroma syntax highlighting classes
func CodeFromLexer(lexer chroma.Lexer, code string) string {
	formatter := html.New(html.WithClasses(true),
		html.WithLineNumbers(false),
		html.PreventSurroundingPre(true),
	)

	htmlbuf := bytes.Buffer{}
	htmlw := bufio.NewWriter(&htmlbuf)

	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		log.Error("Can't tokenize code: %v", err)
		return code
	}
	// style not used for live site but need to pass something
	err = formatter.Format(htmlw, githubStyles, iterator)
	if err != nil {
		log.Error("Can't format code: %v", err)
		return code
	}

	_ = htmlw.Flush()
	// Chroma will add newlines for certain lexers in order to highlight them properly
	// Once highlighted, strip them here, so they don't cause copy/paste trouble in HTML output
	return strings.TrimSuffix(htmlbuf.String(), "\n")
}

// wrapeReader to make sure it can read enough data
type wrapeReader struct {
	reader io.Reader
}

func (r *wrapeReader) Read(p []byte) (n int, err error) {
	offset := 0

	for offset < len(p) {
		len, err := r.reader.Read(p[offset:])
		if err != nil {
			return offset, err
		}

		offset += len
	}

	return offset, nil
}

// File returns a slice of chroma syntax highlighted HTML lines of code and the matched lexer name
func File(fileName, language string, codeReader io.Reader, size int) ([]string, string, error) {
	NewContext()

	if size > sizeLimit {
		return PlainText(codeReader), "", nil
	}

	codeReader = &wrapeReader{
		reader: codeReader,
	}

	var lexer chroma.Lexer
	const blockSize = 8 * 1024

	buffer := make([]byte, blockSize)
	readLen := 0

	// provided language overrides everything
	if language != "" {
		lexer = lexers.Get(language)
	}

	if lexer == nil {
		if val, ok := highlightMapping[filepath.Ext(fileName)]; ok {
			lexer = lexers.Get(val)
		}
	}

	if lexer == nil {
		var err error
		readLen, err = codeReader.Read(buffer)
		if err != nil && err != io.EOF {
			return nil, "", err
		}

		guessLanguage := analyze.GetCodeLanguage(fileName, buffer[:readLen])

		lexer = lexers.Get(guessLanguage)
		if lexer == nil {
			lexer = lexers.Match(fileName)
			if lexer == nil {
				lexer = lexers.Fallback
			}
		}
	}

	if lexer == nil {
		return nil, "", errors.New("unknow lexer")
	}

	realLexer, ok := lexer.(*chroma.RegexLexer)
	if !ok {
		return nil, "", errors.New("unknow lexer")
	}

	state, err := realLexer.NewLexerStateStream(nil, codeReader, blockSize, size)
	if err != nil {
		return nil, "", err
	}
	if readLen > 0 {
		_ = state.AddPreReadenData(buffer[:readLen])
	}

	htmlBuf := &bytes.Buffer{}
	formatter := html.New(html.WithClasses(true),
		html.WithLineNumbers(false),
		html.PreventSurroundingPre(true),
	)
	lines := make([]string, 0, 10)
	lineToken := make([]chroma.Token, 0, 5)

	genLines := func(isEnd bool) error {
		if len(lineToken) == 0 {
			return nil
		}

		if isEnd && len(lineToken) == 1 && lineToken[0].Value == "" {
			return nil
		}

		htmlBuf.Reset()
		err = formatter.Format(htmlBuf, githubStyles, chroma.Literator(lineToken...))
		if err != nil {
			return fmt.Errorf("can't format code: %w", err)
		}

		lines = append(lines, htmlBuf.String())

		return nil
	}

	for {
		token := state.Iterator()
		if token == chroma.EOF {
			err = genLines(true)
			if err != nil {
				return nil, "", err
			}

			break
		}

		for strings.Contains(token.Value, "\n") {
			parts := strings.SplitAfterN(token.Value, "\n", 2)
			// Token becomes the tail.
			token.Value = parts[1]

			// Append the head to the line and flush the line.
			clone := token.Clone()
			clone.Value = parts[0]
			lineToken = append(lineToken, clone)

			err = genLines(false)
			if err != nil {
				return nil, "", err
			}

			lineToken = make([]chroma.Token, 0, 5)
		}

		lineToken = append(lineToken, token)
	}

	lexerName := formatLexerName(lexer.Config().Name)

	return lines, lexerName, nil
}

// PlainText returns non-highlighted HTML for code
func PlainText(codeReader io.Reader) []string {
	r := bufio.NewReader(codeReader)
	m := make([]string, 0, 64)
	for {
		content, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			log.Error("failed to read string from buffer: %v", err)
			break
		}
		if content == "" && err == io.EOF {
			break
		}
		s := gohtml.EscapeString(content)
		m = append(m, s)
	}
	return m
}

func formatLexerName(name string) string {
	if name == "fallback" {
		return "Plaintext"
	}

	return util.ToTitleCaseNoLower(name)
}
