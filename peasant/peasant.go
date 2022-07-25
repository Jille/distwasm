package main

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
	"github.com/tetratelabs/wazero/wasi_snapshot_preview1"
)

func main() {
	log.SetFlags(log.Lshortfile)
	ctx := context.Background()

	r := wazero.NewRuntimeWithConfig(wazero.NewRuntimeConfig().WithWasmCore2())
	defer r.Close(ctx)

	if _, err := r.NewModuleBuilder("env").Instantiate(ctx, r); err != nil {
		log.Panicln(err)
	}

	// Combine the above into our baseline config, overriding defaults.
	config := wazero.NewModuleConfig().WithStdin(os.Stdin).WithStdout(os.Stdout).WithStderr(os.Stderr).WithArgs("peasant").WithSysWalltime().WithSysNanotime().WithSysNanosleep()

	// Instantiate WASI, which implements system I/O such as console output.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		log.Panicln(err)
	}

	bin, err := readPacket()
	if err != nil {
		log.Fatalf("Failed to read binary: %v", err)
	}

	// Compile the WebAssembly module using the default configuration.
	code, err := r.CompileModule(ctx, bin, wazero.NewCompileConfig())
	if err != nil {
		log.Fatalf("Failed to compile binary: %v", err)
	}

	mod, err := r.InstantiateModule(ctx, code, config)
	if err != nil {
		// Note: Most compilers do not exit the module after running "_start",
		// unless there was an error. This allows you to call exported functions.
		if exitErr, ok := err.(*sys.ExitError); ok {
			os.Exit(int(exitErr.ExitCode()))
		} else if !ok {
			log.Panicln(err)
		}
	}

	f := mod.ExportedFunction("processStdio")

	for {
		var b [4]byte
		if _, err := io.ReadFull(os.Stdin, b[:]); err != nil {
			if err == io.EOF {
				return
			}
			log.Fatalf("Failed to read size header: %v", err)
		}
		size := binary.LittleEndian.Uint32(b[:])
		_, err := f.Call(ctx, uint64(size))
		if err != nil {
			log.Fatalf("Failed to call processFromStdio: %v", err)
		}
	}
}

func readPacket() ([]byte, error) {
	var b [4]byte
	if _, err := io.ReadFull(os.Stdin, b[:]); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(b[:])
	msg := make([]byte, size)
	if _, err := io.ReadFull(os.Stdin, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
