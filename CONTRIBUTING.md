# Contributing

Discuss public API or storage-format changes before implementing them. Keep pull
requests focused and explain the behavior being changed and how it was tested.

Before submitting:

```sh
gofmt -w .
go mod tidy
go vet ./...
go test -race -shuffle=on -count=1 ./...
```

For real Redis integration checks, use an isolated server and the environment
variable documented in [README.md](README.md); never use production data.

Add regression tests for fixes and runnable examples for new public APIs.
Do not commit credentials, coverage output, or local editor settings. Contributions
are provided under the repository's [MIT license](LICENSE).
