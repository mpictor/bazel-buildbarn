package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/EdSchouten/bazel-buildbarn/pkg/ac"
	"github.com/EdSchouten/bazel-buildbarn/pkg/blobstore"
	"github.com/EdSchouten/bazel-buildbarn/pkg/builder"
	"github.com/EdSchouten/bazel-buildbarn/pkg/cas"
	"github.com/EdSchouten/bazel-buildbarn/pkg/proto/scheduler"
	"github.com/EdSchouten/bazel-buildbarn/pkg/util"
	"github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"google.golang.org/grpc"
)

func main() {
	var (
		browserURLString = flag.String("browser-url", "http://bbb-browser/", "URL of the Bazel Buildbarn Browser, accessible by the user through 'bazel build --verbose_failures'")
		blobstoreConfig  = flag.String("blobstore-config", "/config/blobstore.conf", "Configuration for blob storage")
		schedulerAddress = flag.String("scheduler", "", "Address of the scheduler to which to connect")
	)
	flag.Parse()

	browserURL, err := url.Parse(*browserURLString)
	if err != nil {
		log.Fatal("Failed to parse browser URL: ", err)
	}

	// Respect file permissions that we pass to os.OpenFile(), os.Mkdir(), etc.
	syscall.Umask(0)

	// Web server for metrics and profiling.
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Fatal(http.ListenAndServe(":80", nil))
	}()

	// Storage access.
	contentAddressableStorageBlobAccess, actionCacheBlobAccess, err := blobstore.CreateBlobAccessObjectsFromConfig(*blobstoreConfig)
	if err != nil {
		log.Fatal("Failed to create blob access: ", err)
	}

	// On-disk caching of content for efficient linking into build environments.
	err = os.Mkdir("/cache", 0755)
	if os.IsExist(err) {
		err = nil
	}
	if err != nil {
		log.Fatal("Failed to create cache directory: ", err)
	}

	contentAddressableStorage := cas.NewDirectoryCachingContentAddressableStorage(
		cas.NewHardlinkingContentAddressableStorage(
			cas.NewBlobAccessContentAddressableStorage(
				blobstore.NewExistencePreconditionBlobAccess(
					contentAddressableStorageBlobAccess)),
			util.DigestKeyWithoutInstance, "/cache", 10000, 1<<30),
		util.DigestKeyWithoutInstance, 1000)
	buildExecutor := builder.NewServerLogInjectingBuildExecutor(
		builder.NewCachingBuildExecutor(
			builder.NewLocalBuildExecutor(contentAddressableStorage),
			ac.NewBlobAccessActionCache(
				blobstore.NewMetricsBlobAccess(actionCacheBlobAccess, "ac_build_executor"))),
		contentAddressableStorage,
		browserURL)

	// Create connection with scheduler.
	schedulerConnection, err := grpc.Dial(
		*schedulerAddress,
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(grpc_prometheus.UnaryClientInterceptor),
		grpc.WithStreamInterceptor(grpc_prometheus.StreamClientInterceptor))
	if err != nil {
		log.Fatal("Failed to create scheduler RPC client: ", err)
	}
	schedulerClient := scheduler.NewSchedulerClient(schedulerConnection)

	// Repeatedly ask the scheduler for work.
	for {
		err := subscribeAndExecute(schedulerClient, buildExecutor, browserURL)
		log.Print("Failed to subscribe and execute: ", err)
		time.Sleep(time.Second * 3)
	}
}

func subscribeAndExecute(schedulerClient scheduler.SchedulerClient, buildExecutor builder.BuildExecutor, browserURL *url.URL) error {
	stream, err := schedulerClient.GetWork(context.Background())
	if err != nil {
		return err
	}
	defer stream.CloseSend()

	for {
		request, err := stream.Recv()
		if err != nil {
			return err
		}

		// Print URL of the action into the log before execution.
		actionURL, err := browserURL.Parse(
			fmt.Sprintf(
				"/action/%s/%s/%d/",
				request.InstanceName,
				request.ActionDigest.Hash,
				request.ActionDigest.SizeBytes))
		if err != nil {
			return err
		}
		log.Print("Action: ", actionURL.String())

		response, _ := buildExecutor.Execute(stream.Context(), request)
		log.Print("ExecuteResponse: ", response)
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}
