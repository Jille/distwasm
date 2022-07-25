package main

import (
	"context"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	pb "github.com/Jille/distwasm/proto"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
)

type void = struct{}

var (
	kingAddr      = pflag.StringP("king-address", "k", "", "gRPC address of the king")
	projectName   = pflag.String("project", "p", "Name of your project")
	minRam        = pflag.IntP("min-ram", "m", 1, "Number of megabytes needed for running the job")
	onePerMachine = pflag.Bool("one-per-machine", false, "Whether to run only job per volunteering machine")
	binaryFile    = pflag.StringP("binary", "b", "", "WASM binary")

	inputDir  = pflag.String("input", "input", "Input directory")
	outputDir = pflag.String("output", "output", "Output directory")
)

func main() {
	log.SetFlags(log.Lshortfile)
	pflag.Parse()
	ctx := context.Background()
	c, err := grpc.Dial(*kingAddr, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Failed to connect to king: %v", err)
	}
	client := pb.NewKingServiceClient(c)

	fns, err := os.ReadDir(*inputDir)
	if err != nil {
		log.Fatalf("Failed to read input directory %q: %v", *inputDir, err)
	}
	var work []string
	for _, fn := range fns {
		if _, err := os.Stat(filepath.Join(*outputDir, fn.Name())); err != nil {
			work = append(work, fn.Name())
		}
	}
	if len(work) == 0 {
		log.Fatalf("No work remaining?")
	}

	bin, err := ioutil.ReadFile(*binaryFile)
	if err != nil {
		log.Fatalf("Failed to read wasm binary %q: %v", *binaryFile, err)
	}

	if *projectName == "" {
		log.Fatalf("Flag --project (-p) is required")
	}

	stream, err := client.SubmitJob(ctx)
	if err != nil {
		log.Fatalf("Failed to submit job: %v", err)
	}
	if err := stream.Send(&pb.SubmitJobRequest{
		Req: &pb.SubmitJobRequest_Job{
			Job: &pb.Job{
				ProjectName:   *projectName,
				Wasm:          bin,
				NumWork:       int32(len(work)),
				MinRamMb:      int32(*minRam),
				OnePerMachine: *onePerMachine,
			},
		},
	}); err != nil {
		log.Fatalf("Failed to submit job: %v", err)
	}
	go func() {
		for i, fn := range work {
			b, err := ioutil.ReadFile(filepath.Join(*inputDir, fn))
			if err != nil {
				log.Printf("Failed to read input file %q: %v", fn, err)
				continue
			}
			if err := stream.Send(&pb.SubmitJobRequest{
				Req: &pb.SubmitJobRequest_AddWork{
					AddWork: &pb.WorkRequest{
						Id:   int32(i + 1),
						Data: b,
					},
				},
			}); err != nil {
				log.Fatalf("Failed to submit work: %v", err)
			}
		}
		if err := stream.CloseSend(); err != nil {
			log.Fatalf("Failed to CloseSend")
		}
	}()
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("Error while reading from king: %v", err)
		}
		if resp.GetCompletedWork() != nil {
			id := resp.GetCompletedWork().GetId()
			fn := work[id-1]
			if resp.GetCompletedWork().GetErrorMessage() != "" {
				log.Printf("Job %q failed: %v", fn, resp.GetCompletedWork().GetErrorMessage())
				continue
			}
			if err := ioutil.WriteFile(filepath.Join(*outputDir, fn)+".tmp", resp.GetCompletedWork().GetResult(), 0666); err != nil {
				log.Printf("Failed to write result: %v", err)
				continue
			}
			if err := os.Rename(filepath.Join(*outputDir, fn)+".tmp", filepath.Join(*outputDir, fn)); err != nil {
				log.Printf("Failed to rename result: %v", err)
				continue
			}
		}
	}
}
