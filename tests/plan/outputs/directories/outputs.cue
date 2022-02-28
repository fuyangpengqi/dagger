package main

import (
	"dagger.io/dagger"
)

dagger.#Plan & {
	actions: data: dagger.#WriteFile & {
		input:       dagger.#Scratch
		path:        "/test_outputs"
		permissions: 0o600
		contents:    "foobar"
	}

	outputs: directories: test_outputs: {
		contents: actions.data.output
		dest:     "./out"
	}
}
