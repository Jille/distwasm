package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	pb "github.com/Jille/distwasm/proto"
	"github.com/Jille/rpcz"
	"github.com/spf13/pflag"
	"go.uber.org/atomic"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var (
	port = pflag.IntP("port", "p", 1900, "Port number to serve on")

	mtx        sync.Mutex
	cond       = sync.NewCond(&mtx)
	currentJob *activeJob
)

func main() {
	log.SetFlags(log.Lshortfile)
	pflag.Parse()
	go func() {
		http.Handle("/rpcz", rpcz.Handler)
		http.ListenAndServe(":1901", nil)
	}()
	s := grpc.NewServer(grpc.StreamInterceptor(rpcz.StreamServerInterceptor))
	reflection.Register(s)
	pb.RegisterKingServiceServer(s, &service{})

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port %d: %v", *port, err)
	}
	if err := s.Serve(l); err != nil {
		log.Fatalf("Failed to Serve(): %v", err)
	}
}

type service struct{}

func (s *service) Volunteer(stream pb.KingService_VolunteerServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := msg.GetHello()
	log.Printf("%s reported for duty with %d cpus and %d MB RAM", hello.GetHostname(), hello.GetCpus(), hello.GetRamMbAllocatable())
	var jmtx sync.Mutex
	var j *activeJob
	outstandingWork := semaphore.NewWeighted(int64(hello.GetCpus()))
	if err := outstandingWork.Acquire(ctx, int64(hello.GetCpus())); err != nil {
		return err
	}
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				cancel()
				return
			}
			jmtx.Lock()
			j := j
			jmtx.Unlock()
			outstandingWork.Release(1)
			if msg.GetFinishWork() != nil {
				j.workResponses <- msg.GetFinishWork()
			} else if msg.GetJobError() != "" {
				log.Fatalf("Job errors are yet unhandled: %v", msg)
			}
		}
	}()
	for {
		mtx.Lock()
		for currentJob == nil || currentJob == j {
			cond.Wait()
		}
		tmp := currentJob
		mtx.Unlock()
		jmtx.Lock()
		j = tmp
		jmtx.Unlock()

		if err := stream.Send(&pb.VolunteerResponse{
			Resp: &pb.VolunteerResponse_StartJob{
				StartJob: j.job,
			},
		}); err != nil {
			return err
		}

		numPeasants := 1
		if !j.job.GetOnePerMachine() {
			numPeasants = int(hello.GetCpus())
			if m := int(hello.GetRamMbAllocatable()) / int(j.job.GetMinRamMb()); m < numPeasants {
				numPeasants = m
			}
		}
		outstandingWork.Release(int64(numPeasants))
		var wg sync.WaitGroup
		wg.Add(numPeasants)
		for i := 0; numPeasants > i; i++ {
			go func() {
				defer wg.Done()
				for {
					if outstandingWork.Acquire(ctx, 1) != nil {
						return
					}
					select {
					case <-ctx.Done():
						outstandingWork.Release(1)
						return
					case w, ok := <-j.workRequests:
						if !ok {
							outstandingWork.Release(1)
							return
						}
						if err := stream.Send(&pb.VolunteerResponse{
							Resp: &pb.VolunteerResponse_StartWork{
								StartWork: w,
							},
						}); err != nil {
							cancel()
							outstandingWork.Release(1)
							return
						}
					}
				}
			}()
		}
		wg.Wait()
		outstandingWork.Acquire(ctx, int64(numPeasants))

		if err := stream.Send(&pb.VolunteerResponse{
			Resp: &pb.VolunteerResponse_EndJob_{
				EndJob: &pb.VolunteerResponse_EndJob{},
			},
		}); err != nil {
			return err
		}
	}
}

func (s *service) SubmitJob(stream pb.KingService_SubmitJobServer) error {
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	job := msg.GetJob()
	log.Printf("New job %q", job.GetProjectName())
	aj := &activeJob{
		job:           job,
		workRequests:  make(chan *pb.WorkRequest),
		workResponses: make(chan *pb.WorkResponse),
	}
	mtx.Lock()
	for currentJob != nil {
		cond.Wait()
	}
	currentJob = aj
	cond.Broadcast()
	mtx.Unlock()
	defer func() {
		mtx.Lock()
		currentJob = nil
		cond.Broadcast()
		mtx.Unlock()
	}()
	openWork := atomic.NewInt32(0)
	openWork.Inc() // One extra until the stream is closed.
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					openWork.Dec()
					close(aj.workRequests)
					return
				}
				log.Printf("Not handled: Losing connection from SubmitJob: %v", err)
				return
			}
			openWork.Inc()
			aj.workRequests <- msg.GetAddWork()
		}
	}()
	for openWork.Load() > 0 {
		w := <-aj.workResponses
		openWork.Dec()
		if err := stream.Send(&pb.SubmitJobResponse{
			Resp: &pb.SubmitJobResponse_CompletedWork{
				CompletedWork: w,
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

type activeJob struct {
	job           *pb.Job
	workRequests  chan *pb.WorkRequest
	workResponses chan *pb.WorkResponse
}
