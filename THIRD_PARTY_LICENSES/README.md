# Third-Party Licenses

This directory contains the license and notice files for Go packages linked
into Mamari release binaries. It was generated with:

```bash
go run github.com/google/go-licenses@v1.6.0 save ./... \
  --save_path=THIRD_PARTY_LICENSES
```

Regenerate and review this directory whenever `go.mod` changes. Mamari's own
license is [`../LICENSE`](../LICENSE).
