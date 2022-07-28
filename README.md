# distwasm

distwasm is a distributed system that spreads your computation over different manchines and cores. You can submit WebAssembly that'll be run on all machines.

## Architecture

The **king** is the central point of coordination. It receives jobs from the queen and tells the dukes to spawn peasants to execute it. It listens on a gRPC TCP port and the dukes and queens open streaming RPCs to it.

Each machine runs a single **duke**, which will spawn one or more peasants. The duke manages the peasants' lifecycles and communicates with the king.

The **peasant** is the low-level worker. It contains a WASM engine, and does little more than compiling the WASM program and then executing it. It communicates with the duke over stdin/stdout. Their protocol is to send a little-endian uint32 with the length of the blob, and then a blob of data. The first blob is the WASM to be compiled, all future blobs are input for the WASM program. The program should respond with a uint32 length and then the response.

The **queen** is a CLI binary that submits jobs to the king and receives the response.

A **job** is a single WASM program and a few settings. The king distributes it to the dukes, who will start peasants that compile it and then start processing *work*.

A unit of ***work*** is some input to be passed through the WASM program. The queen sends work to the king (as part of a job), who then distributes it over the dukes, who distribute it over their peasants and the peasants call a function in the WASM program. The result is passed back to the queen.

## Example

```
go run ./king &
go run ./duke -k localhost:1900 &
mkdir my-input my-output
for i in $(seq 10000); do echo "$RANDOM + $RANDOM" > "my-input/$i.txt"; done
tinygo build -o examples/summer/summer.wasm -target wasi -opt 2 ./examples/summer/summer.go
go run ./queen -b examples/summer/summer.wasm -k localhost:1900 --project=summer --input=my-input --output=my-output
head my-output/*
```
