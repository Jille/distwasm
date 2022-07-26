package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	pb "github.com/Jille/distwasm/proto"
	"github.com/Jille/rpcz"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"golang.org/x/exp/slices"
)

type void = struct{}

var (
	kingAddr = pflag.StringP("king-address", "k", "", "gRPC address of the king")
	hostname = pflag.String("hostname", myHostname(), "Your hostname")
	numCPUs  = pflag.Int("num-cpus", runtime.NumCPU(), "Number of CPUs we can use")
	ramLimit = pflag.Int("ram-limit", 256, "Number of megabytes you're willing to give up")
)

func myHostname() string {
	h, err := os.Hostname()
	if err != nil {
		panic(fmt.Errorf("Can't get hostname: %v", err))
	}
	return h
}

func main() {
	log.SetFlags(log.Lshortfile)
	pflag.Parse()
	go func() {
		http.Handle("/rpcz", rpcz.Handler)
		http.ListenAndServe(":1902", nil)
	}()
	c, err := grpc.Dial(*kingAddr, grpc.WithInsecure(), grpc.WithChainStreamInterceptor(rpcz.StreamClientInterceptor))
	if err != nil {
		log.Fatalf("Failed to connect to king: %v", err)
	}
	client := pb.NewKingServiceClient(c)

	for {
		if err := volunteer(client); err != nil {
			log.Printf("Volunteering failed: %v", err)
		}
		time.Sleep(10 * time.Second)
	}
}

func volunteer(client pb.KingServiceClient) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Volunteer(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.VolunteerRequest{
		Req: &pb.VolunteerRequest_Hello_{
			Hello: &pb.VolunteerRequest_Hello{
				Hostname:         *hostname,
				Cpus:             int32(*numCPUs),
				RamMbAllocatable: int32(*ramLimit),
			},
		},
	}); err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := runJob(ctx, stream, resp.GetStartJob()); err != nil {
			return err
		}
	}
}

func runJob(ctx context.Context, stream pb.KingService_VolunteerClient, job *pb.Job) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	numPeasants := 1
	if !job.GetOnePerMachine() {
		numPeasants = *numCPUs
		if m := *ramLimit / int(job.GetMinRamMb()); m < numPeasants {
			numPeasants = m
		}
	}
	log.Printf("Preparing job %q for %d peasants", job.GetProjectName(), numPeasants)
	barons := make([]*baron, numPeasants)
	var respondMtx sync.Mutex
	readyBarons := make(chan *baron, numPeasants)
	for i := 0; numPeasants > i; i++ {
		barons[i] = newBaron(ctx, job, readyBarons, func(resp *pb.VolunteerRequest) {
			respondMtx.Lock()
			defer respondMtx.Unlock()
			if err := ctx.Err(); err != nil {
				return
			}
			_ = stream.Send(resp)
		})
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		if resp.GetEndJob() != nil {
			// Returning cancels the context which kills the peasants.
			return nil
		}
		log.Printf("Got work (%d: %q). Finding baron", resp.GetStartWork().GetId(), string(resp.GetStartWork().GetData()))
		b := <-readyBarons
		log.Printf("Found baron %p", b)
		b.workCh <- resp.GetStartWork()
		log.Printf("Sent work to baron")
	}
}

// A baron gives work to a peasant and whips it back to life when it dies.
type baron struct {
	job         *pb.Job
	workCh      chan *pb.WorkRequest
	deathCh     chan error
	respond     func(*pb.VolunteerRequest)
	readyBarons chan *baron
}

func newBaron(ctx context.Context, job *pb.Job, readyBarons chan *baron, respond func(*pb.VolunteerRequest)) *baron {
	b := &baron{
		job:         job,
		workCh:      make(chan *pb.WorkRequest),
		deathCh:     make(chan error),
		respond:     respond,
		readyBarons: readyBarons,
	}
	go b.runner(ctx)
	return b
}

func (b *baron) runner(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := b.run(ctx); err != nil {
			if err := ctx.Err(); err != nil {
				// We don't care about it anymore anyway.
				return
			}
			b.respond(&pb.VolunteerRequest{
				Req: &pb.VolunteerRequest_JobError{
					JobError: err.Error(),
				},
			})
			log.Fatalf("Job error: %v", err)
		}
	}
}

func (b *baron) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := &pokingBuffer{
		ctx: ctx,
		ch:  make(chan []byte),
	}
	stderr := &pokingBuffer{
		ctx: ctx,
		ch:  make(chan []byte),
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "../peasant/peasant")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	/*
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Pdeathsig: syscall.SIGKILL,
		}
	*/
	if err := cmd.Start(); err != nil {
		return err
	}
	binary.Write(stdin, binary.LittleEndian, uint32(len(b.job.GetWasm())))
	if _, err := stdin.Write(b.job.GetWasm()); err != nil {
		return err
	}
	go func() {
		b.deathCh <- cmd.Wait()
	}()

waitForReadyLoop:
	for {
		select {
		case <-b.deathCh:
			if err := ctx.Err(); err != nil {
				// The peasant is expected to die when the context is cancelled, so we ignore this error.
				return nil
			}
			msg := "Process died when starting ("
			if err == nil {
				msg += "exit code 0"
			} else {
				msg += err.Error()
			}
			msg += ")"
			if e := stderrBuf.String(); e != "" {
				msg += "\n\n" + e
			}
			return errors.New(msg)

		case p := <-stdout.ch:
			stdoutBuf.Write(p)
			if stdoutBuf.Len() < 4 {
				break
			}
			b := stdoutBuf.Bytes()
			if !bytes.Equal(b, []byte{0, 0, 0, 0}) {
				return fmt.Errorf("unexpected data on stdout when starting: %q", string(b))
			}
			stdoutBuf.Reset()
			break waitForReadyLoop

		case p := <-stderr.ch:
			stderrBuf.Write(p)
			return fmt.Errorf("unexpected data on stderr when starting: %q", stderrBuf.String())
		}
	}

	var activeWork *pb.WorkRequest

	for {
		log.Printf("Baron %p reporting for duty", b)
		b.readyBarons <- b
	waitForWorkLoop:
		for {
			select {
			case activeWork = <-b.workCh:
				binary.Write(stdin, binary.LittleEndian, uint32(len(activeWork.GetData())))
				if _, err := stdin.Write(activeWork.GetData()); err != nil {
					return err
				}
				log.Printf("Baron %p delegated to the peasant", b)
				break waitForWorkLoop

			case <-b.deathCh:
				if err := ctx.Err(); err != nil {
					// The peasant is expected to die when the context is cancelled, so we ignore this error.
					return nil
				}
				msg := "Process died while idle without output ("
				if err == nil {
					msg += "exit code 0"
				} else {
					msg += err.Error()
				}
				msg += ")"
				return errors.New(msg)

			case p := <-stdout.ch:
				stdoutBuf.Write(p)
				return fmt.Errorf("unexpected data on stdout while idle: %q", stdoutBuf.String())
			case p := <-stderr.ch:
				stderrBuf.Write(p)
				return fmt.Errorf("unexpected data on stderr while idle: %q", stderrBuf.String())
			}
		}

	executeWorkLoop:
		for {
			select {
			case err := <-b.deathCh:
				var msg string
				if e := stderrBuf.String(); e != "" {
					msg += e + "\n\n"
				}
				if err == nil {
					msg += "Exit code 0"
				} else {
					msg += fmt.Sprintf("Exit: %v", err)
				}
				b.respond(&pb.VolunteerRequest{
					Req: &pb.VolunteerRequest_FinishWork{
						FinishWork: &pb.WorkResponse{
							Id:           activeWork.Id,
							ErrorMessage: msg,
						},
					},
				})
				return nil
			case p := <-stdout.ch:
				stdoutBuf.Write(p)
				log.Printf("Got some bytes from the peasant: %d", stdoutBuf.Len())
				if stdoutBuf.Len() < 4 {
					break
				}
				o := stdoutBuf.Bytes()
				l := int(binary.LittleEndian.Uint32(o[:4]))
				if len(o)-4 < l {
					break
				}
				o = o[4 : 4+l]
				log.Printf("Reporting job completion")
				b.respond(&pb.VolunteerRequest{
					Req: &pb.VolunteerRequest_FinishWork{
						FinishWork: &pb.WorkResponse{
							Id:           activeWork.Id,
							ErrorMessage: stderrBuf.String(),
							Result:       o,
						},
					},
				})
				activeWork = nil
				stdoutBuf.Reset()
				stderrBuf.Reset()
				break executeWorkLoop
			case p := <-stderr.ch:
				stderrBuf.Write(p)
			}
		}
	}
}

type pokingBuffer struct {
	ctx context.Context
	ch  chan []byte
}

var _ io.Writer = &pokingBuffer{}

func (p *pokingBuffer) Write(b []byte) (int, error) {
	select {
	case p.ch <- slices.Clone(b):
	case <-p.ctx.Done():
	}
	return len(b), nil
}
