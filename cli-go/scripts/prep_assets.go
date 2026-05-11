// Pre-archive hook for goreleaser.
//
// Copies LICENSE + the OpenAPI / AsyncAPI specs from the repo root
// into the cli-go/ working directory so the goreleaser archive step
// can reference them by plain (relative) name. This dodges a known
// goreleaser path-handling bug on Windows where `../foo` resolves
// to `./C:/...` when the working directory is on a Windows drive.
//
// Invoked via the before.hooks section of .goreleaser.yml:
//
//	before:
//	  hooks:
//	    - go run ./scripts/prep_assets.go
//
// The copied files (LICENSE, openapi.yaml, asyncapi.yaml at cli-go/)
// are gitignored.
package main

import (
	"io"
	"log"
	"os"
)

func cp(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		log.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		log.Fatalf("create %s: %v", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		log.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func main() {
	cp("../LICENSE", "LICENSE")
	cp("../openapi/openapi.yaml", "openapi.yaml")
	cp("../openapi/asyncapi.yaml", "asyncapi.yaml")
	log.Println("prep_assets: copied LICENSE + openapi + asyncapi into cli-go/")
}
