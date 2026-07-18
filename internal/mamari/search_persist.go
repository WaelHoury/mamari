package mamari

import (
	"bytes"
	"encoding/gob"
	"os"
	"sort"
	"strings"
)

// searchSidecarBinaryMagic mirrors indexBinaryMagic's role for the main
// index (index.go): a header so loadCodeSearchSidecar can tell a real
// sidecar from anything else without depending on file extension.
var searchSidecarBinaryMagic = []byte("mamari-search-v1\n")

type persistedCodeSearchIndex struct {
	SchemaVersion int
	FileHashes    map[string]string
	Files         []persistedCodeSearchFile
}

type persistedCodeSearchFile struct {
	File      string
	PostingID uint32
	// Language, PathTokens, BaseTokens are kept; AllTokens is deliberately
	// NOT persisted — it's fully derivable from PathTokens/BaseTokens plus
	// each line's Tokens/SymbolText (see buildCodeSearchFile), and storing it
	// duplicated the bulk of a file's token data a second time for no
	// benefit. TokenCount (the BM25 "document length" — see
	// bm25LengthNorm in search_code.go) IS persisted: it's cheap and silently
	// dropping it would degrade ranking quality on sidecar-loaded files
	// (bm25LengthNorm treats a zero token count as a no-op).
	Language   string
	PathTokens []string
	BaseTokens []string
	TokenCount int
	Lines      []persistedCodeSearchLine
}

type persistedCodeSearchLine struct {
	Number     int
	Text       string
	Tokens     []string
	Symbols    []CGPSymbolSummary
	SymbolText []string
}

// saveCodeSearchSidecar persists idx's tokenized search-code file cache as a
// compact gob-encoded binary. Go's encoding/json is markedly slower than gob
// to decode at multi-megabyte sizes, which on a
// real repo made the JSON-encoded sidecar provide no load-time benefit over
// rebuilding from source.
func saveCodeSearchSidecar(idx *Index, path string) error {
	files := idx.ensureCodeSearchIndex()
	if len(files) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	snap := idx.snapshot()
	payload := persistedCodeSearchIndex{
		SchemaVersion: SchemaVersion,
		FileHashes:    map[string]string{},
		Files:         make([]persistedCodeSearchFile, 0, len(files)),
	}
	for _, meta := range snap.Files {
		if searchableCodeLanguage(meta.Language) {
			payload.FileHashes[meta.Path] = meta.SHA256
		}
	}
	for _, file := range files {
		pf := persistedCodeSearchFile{
			File:       file.file,
			PostingID:  file.postingID,
			Language:   file.language,
			PathTokens: mapKeys(file.pathTokens),
			BaseTokens: mapKeys(file.baseTokens),
			TokenCount: file.tokenCount,
			Lines:      make([]persistedCodeSearchLine, 0, len(file.lines)),
		}
		for lineIndex, line := range file.lines {
			pf.Lines = append(pf.Lines, persistedCodeSearchLine{
				Number:     lineIndex + 1,
				Text:       file.lineText(line),
				Tokens:     line.tokens.strings(),
				Symbols:    dereferenceSymbolSummaries(file.lineSymbols(line, nil)),
				SymbolText: line.symbolText.strings(),
			})
		}
		payload.Files = append(payload.Files, pf)
	}
	var buf bytes.Buffer
	buf.Write(searchSidecarBinaryMagic)
	if err := gob.NewEncoder(&buf).Encode(&payload); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// loadCodeSearchSidecar reads path (if it exists, matches the current
// SchemaVersion, and its FileHashes match idx's current file content
// exactly — same staleness contract as before) and, on success, populates
// idx.codeSearchFiles/codeSearchPostings/codeSearchTermDocFreq directly so
// the caller can skip rebuilding the search index from source entirely.
// Reports whether it loaded a usable index.
func loadCodeSearchSidecar(idx *Index, path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, searchSidecarBinaryMagic) {
		return false
	}
	var payload persistedCodeSearchIndex
	if err := gob.NewDecoder(bytes.NewReader(data[len(searchSidecarBinaryMagic):])).Decode(&payload); err != nil {
		return false
	}
	if payload.SchemaVersion != SchemaVersion || !codeSearchSidecarMatches(idx, payload.FileHashes) {
		return false
	}
	files := make([]codeSearchFile, 0, len(payload.Files))
	for i, pf := range payload.Files {
		postingID := pf.PostingID
		if postingID == 0 {
			postingID = uint32(i + 1)
		}
		cf := codeSearchFile{
			file:       pf.File,
			language:   pf.Language,
			postingID:  postingID,
			pathTokens: sliceSet(pf.PathTokens),
			baseTokens: sliceSet(pf.BaseTokens),
			tokenCount: pf.TokenCount,
			lines:      make([]codeSearchLine, 0, len(pf.Lines)),
		}
		var source strings.Builder
		allTokens := map[string]bool{}
		mergeSearchTokens(allTokens, cf.pathTokens)
		mergeSearchTokens(allTokens, cf.baseTokens)
		// Re-memoize by symbol ID on load too, exactly like
		// searchSymbolsByLine does on a fresh build — the persisted gob
		// format stores one value per line (byte-compatible, simple), but
		// loading each line's symbols independently would silently
		// reintroduce the per-line duplication a sidecar-loaded index is
		// specifically meant to avoid paying for again.
		summaryCache := map[string]*CGPSymbolSummary{}
		summaryIndexes := map[*CGPSymbolSummary]uint32{}
		for _, line := range pf.Lines {
			mergeTokensFromSlice(allTokens, line.Tokens)
			mergeTokensFromSlice(allTokens, line.SymbolText)
			lineSymbols := internSymbolSummaries(line.Symbols, summaryCache)
			symbolStart, symbolCount := appendCodeSearchLineSymbols(&cf, lineSymbols, summaryIndexes)
			textStart := source.Len()
			source.WriteString(line.Text)
			cf.lines = append(cf.lines, codeSearchLine{
				textStart:   uint32(textStart),
				textEnd:     uint32(source.Len()),
				tokenBloom:  compactTokenBloom(line.Tokens),
				symbolBloom: compactTokenBloom(line.SymbolText),
				tokens:      packCompactTokenSet(line.Tokens),
				symbolText:  packCompactTokenSet(line.SymbolText),
				symbolStart: symbolStart,
				symbolCount: symbolCount,
			})
		}
		cf.source = source.String()
		cf.allTokens = packCompactTokenSet(mapKeys(allTokens))
		files = append(files, cf)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].file < files[j].file })
	internCodeSearchFiles(files)

	idx.mu.Lock()
	idx.codeSearchFiles = files
	idx.codeSearchPostings = buildCodeSearchPostings(files)
	idx.codeSearchTermDocFreq = buildCodeSearchTermDocFreq(files)
	idx.codeSearchBuilt = true
	idx.mu.Unlock()
	return true
}

func codeSearchSidecarMatches(idx *Index, hashes map[string]string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	want := map[string]string{}
	for _, meta := range idx.Files {
		if searchableCodeLanguage(meta.Language) {
			want[meta.Path] = meta.SHA256
		}
	}
	if len(want) != len(hashes) {
		return false
	}
	for file, hash := range want {
		if hashes[file] != hash {
			return false
		}
	}
	return true
}

func mapKeys(values map[string]bool) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sliceSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}
