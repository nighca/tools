### Build wasm file for goxls-web

```shell
GOOS=js GOARCH=wasm go build -o ./goxls-web.wasm ../goxls/cmd/goxls-web
```

### Copy wasm execute js file

```shell
cp "$(go env GOROOT)/misc/wasm/wasm_exec.js" .
```