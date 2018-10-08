package main

import (
	"flag"
	"html/template"
	"log"
	"net/http"
	_ "net/http/pprof"
	"path"
	"strings"

	"github.com/EdSchouten/bazel-buildbarn/pkg/ac"
	"github.com/EdSchouten/bazel-buildbarn/pkg/blobstore"
	"github.com/EdSchouten/bazel-buildbarn/pkg/cas"
	"github.com/gorilla/mux"
	"github.com/kballard/go-shellquote"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	var (
		blobstoreConfig = flag.String("blobstore-config", "/config/blobstore.conf", "Configuration for blob storage")
		browsePort      = flag.String("port", ":80", "Browse port")
	)
	flag.Parse()

	// Storage access.
	contentAddressableStorageBlobAccess, actionCacheBlobAccess, err := blobstore.CreateBlobAccessObjectsFromConfig(*blobstoreConfig)
	if err != nil {
		log.Fatal("Failed to create blob access: ", err)
	}

	templates, err := template.New("templates").Funcs(template.FuncMap{
		"basename": path.Base,
		"shellquote": func(in string) string {
			// Use non-breaking hyphens to improve readability of output.
			return strings.Replace(shellquote.Join(in), "-", "‑", -1)
		},
	}).ParseGlob("templates/*")
	if err != nil {
		panic(err)
	}

	router := mux.NewRouter()
	router.Handle("/metrics", promhttp.Handler())
	router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("./static/"))))
	NewBrowserService(
		cas.NewBlobAccessContentAddressableStorage(contentAddressableStorageBlobAccess),
		contentAddressableStorageBlobAccess,
		ac.NewBlobAccessActionCache(actionCacheBlobAccess),
		templates,
		router)
	log.Fatal(http.ListenAndServe(*browsePort, router))
}
