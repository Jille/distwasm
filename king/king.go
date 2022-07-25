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
			if msg.GetFinishWork() != nil {
				j.workResponses <- msg.GetFinishWork()
			} else if msg.GetJobError() != "" {
				log.Fatalf("Job errors are yet unhandled: %v", msg)
			}
		}
	}()
	for {
		mtx.Lock()
		for currentJob == j {
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
		var wg sync.WaitGroup
		wg.Add(numPeasants)
		for i := 0; numPeasants > i; i++ {
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case w, ok := <-j.workRequests:
						if !ok {
							return
						}
						if err := stream.Send(&pb.VolunteerResponse{
							Resp: &pb.VolunteerResponse_StartWork{
								StartWork: w,
							},
						}); err != nil {
							cancel()
							return
						}
					}
				}
			}()
		}
		wg.Wait()
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
		w := <- aj.workResponses
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
	job          *pb.Job
	workRequests chan *pb.WorkRequest

	workResponses chan *pb.WorkResponse
}
