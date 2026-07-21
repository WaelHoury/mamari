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
var searchSidecarBinaryMagic = []byte("mamari-search-v3\n")

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
	// Symbols are stored once per file. The v1 format embedded complete
	// CGPSymbolSummary values on every source line covered by a symbol; a
	// long class or method therefore duplicated the same strings hundreds of
	// times on disk and again while gob decoded it. Lines now retain only
	// fixed-width spans into LineSymbolIndexes, matching the in-memory form.
	Symbols           []CGPSymbolSummary
	LineSymbolIndexes []uint32
	Lines             []persistedCodeSearchLine
}

type persistedCodeSearchLine struct {
	Text        string
	Tokens      string
	SymbolText  string
	SymbolStart uint32
	SymbolCount uint8
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
		if searchableCodeFile(meta, snap.Repo.Root) {
			payload.FileHashes[meta.Path] = meta.SHA256
		}
	}
	for _, file := range files {
		pf := persistedCodeSearchFile{
			File:              file.file,
			PostingID:         file.postingID,
			Language:          file.language,
			PathTokens:        mapKeys(file.pathTokens),
			BaseTokens:        mapKeys(file.baseTokens),
			TokenCount:        file.tokenCount,
			Symbols:           dereferenceSymbolSummaries(file.symbolSummaries),
			LineSymbolIndexes: append([]uint32(nil), file.lineSymbolIndexes...),
			Lines:             make([]persistedCodeSearchLine, 0, len(file.lines)),
		}
		for _, line := range file.lines {
			pf.Lines = append(pf.Lines, persistedCodeSearchLine{
				Text:        file.lineText(line),
				Tokens:      string(line.tokens),
				SymbolText:  string(line.symbolText),
				SymbolStart: line.symbolStart,
				SymbolCount: line.symbolCount,
			})
		}
		payload.Files = append(payload.Files, pf)
	}
	var buf bytes.Buffer
	buf.Write(searchSidecarBinaryMagic)
	if err := gob.NewEncoder(&buf).Encode(&payload); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
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
	postingIDs := make(map[uint32]bool, len(payload.Files))
	filePaths := make(map[string]bool, len(payload.Files))
	for i, pf := range payload.Files {
		if _, ok := payload.FileHashes[pf.File]; !ok || filePaths[pf.File] {
			return false
		}
		filePaths[pf.File] = true
		postingID := pf.PostingID
		if postingID == 0 {
			postingID = uint32(i + 1)
		}
		if postingIDs[postingID] {
			return false
		}
		postingIDs[postingID] = true
		summaryCache := map[string]*CGPSymbolSummary{}
		cf := codeSearchFile{
			file:              pf.File,
			language:          pf.Language,
			postingID:         postingID,
			pathTokens:        sliceSet(pf.PathTokens),
			baseTokens:        sliceSet(pf.BaseTokens),
			tokenCount:        pf.TokenCount,
			symbolSummaries:   internSymbolSummaries(pf.Symbols, summaryCache),
			lineSymbolIndexes: append([]uint32(nil), pf.LineSymbolIndexes...),
			lines:             make([]codeSearchLine, 0, len(pf.Lines)),
		}
		for _, index := range cf.lineSymbolIndexes {
			if int(index) >= len(cf.symbolSummaries) {
				return false
			}
		}
		var source strings.Builder
		allTokens := map[string]bool{}
		mergeSearchTokens(allTokens, cf.pathTokens)
		mergeSearchTokens(allTokens, cf.baseTokens)
		for _, line := range pf.Lines {
			lineTokens := compactTokenSet(line.Tokens)
			symbolText := compactTokenSet(line.SymbolText)
			lineTokens.forEach(func(token string) { allTokens[token] = true })
			symbolText.forEach(func(token string) { allTokens[token] = true })
			symbolEnd := uint64(line.SymbolStart) + uint64(line.SymbolCount)
			if symbolEnd > uint64(len(cf.lineSymbolIndexes)) {
				return false
			}
			textStart := source.Len()
			source.WriteString(line.Text)
			if uint64(source.Len()) > uint64(^uint32(0)) {
				return false
			}
			cf.lines = append(cf.lines, codeSearchLine{
				textStart:   uint32(textStart),
				textEnd:     uint32(source.Len()),
				tokenBloom:  compactTokenSetBloom(lineTokens),
				symbolBloom: compactTokenSetBloom(symbolText),
				tokens:      lineTokens,
				symbolText:  symbolText,
				symbolStart: line.SymbolStart,
				symbolCount: line.SymbolCount,
			})
		}
		cf.source = source.String()
		cf.allTokens = packCompactTokenSet(mapKeys(allTokens))
		files = append(files, cf)
	}
	if len(filePaths) != len(payload.FileHashes) {
		return false
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

func compactTokenSetBloom(tokens compactTokenSet) uint64 {
	var bloom uint64
	tokens.forEach(func(token string) { bloom |= searchTokenBloom(token) })
	return bloom
}

func codeSearchSidecarMatches(idx *Index, hashes map[string]string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	want := map[string]string{}
	for _, meta := range idx.Files {
		if searchableCodeFile(meta, idx.Repo.Root) {
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
