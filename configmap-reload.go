package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/a8m/envsubst"
	fsnotify "github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const version = "0.6"
const namespace = "configmap_reload"

var (
	volumeDirs        volumeDirsFlag
	webhook           webhookFlag
	webhookMethod     = flag.String("webhook-method", "POST", "the HTTP method url to use to send the webhook")
	webhookStatusCode = flag.Int("webhook-status-code", 200, "the HTTP status code indicating successful triggering of reload")
	webhookRetries    = flag.Int("webhook-retries", 1, "the amount of times to retry the webhook reload request")
	listenAddress     = flag.String("web.listen-address", ":9533", "Address to listen on for web interface and telemetry.")
	metricPath        = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	subEnv            = flag.Bool("envsubst", false, "Enable envsubst substitution after reloading the file. Can only be used with ONE file.")
	subDestination    = flag.String("destinationpath", "", "Suffix to prepend to substituted file.")
	// subFile - argument is necessary because the fsnotify events trigger for the `..data` file in the configmap's
	// directory but not the symlinks to individual files. So we need to provide the name of the file we wish to run envsubst on.
	// There are cleaner ways to put this - but it works for now.
	subFile = flag.String("envsubst.file", "", "the filename inside -volume to substitute env vars into.")

	lastReloadError = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "last_reload_error",
		Help:      "Whether the last reload resulted in an error (1 for error, 0 for success)",
	}, []string{"webhook"})
	requestDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "last_request_duration_seconds",
		Help:      "Duration of last webhook request",
	}, []string{"webhook"})
	successReloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "success_reloads_total",
		Help:      "Total success reload calls",
	}, []string{"webhook"})
	requestErrorsByReason = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "request_errors_total",
		Help:      "Total request errors by reason",
	}, []string{"webhook", "reason"})
	watcherErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "watcher_errors_total",
		Help:      "Total filesystem watcher errors",
	})
	requestsByStatusCode = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "requests_total",
		Help:      "Total requests by response status code",
	}, []string{"webhook", "status_code"})
)

func init() {
	prometheus.MustRegister(lastReloadError)
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(successReloads)
	prometheus.MustRegister(requestErrorsByReason)
	prometheus.MustRegister(watcherErrors)
	prometheus.MustRegister(requestsByStatusCode)
}

func main() {
	fmt.Printf("0xVox ConfigMap Reload v%s\n", version)
	flag.Var(&volumeDirs, "volume-dir", "the config map volume directory to watch for updates; may be used multiple times")
	flag.Var(&webhook, "webhook-url", "the url to send a request to when the specified config map volume directory has been updated")
	flag.Parse()

	if len(volumeDirs) < 1 {
		log.Println("Missing volume-dir")
		log.Println()
		flag.Usage()
		os.Exit(1)
	}

	if len(webhook) < 1 {
		log.Println("Missing webhook-url")
		log.Println()
		flag.Usage()
		os.Exit(1)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if !isValidEvent(event) {
					continue
				}
				log.Println("config map updated")
				if *subEnv {
					envsubstFile(event.Name)
				}
				for _, h := range webhook {
					begun := time.Now()
					req, err := http.NewRequest(*webhookMethod, h.String(), nil)
					if err != nil {
						setFailureMetrics(h.String(), "client_request_create")
						log.Println("error:", err)
						continue
					}
					userInfo := h.User
					if userInfo != nil {
						if password, passwordSet := userInfo.Password(); passwordSet {
							req.SetBasicAuth(userInfo.Username(), password)
						}
					}

					successfulReloadWebhook := false

					for retries := *webhookRetries; retries != 0; retries-- {
						log.Printf("performing webhook request (%d/%d)", retries, *webhookRetries)
						resp, err := http.DefaultClient.Do(req)
						if err != nil {
							setFailureMetrics(h.String(), "client_request_do")
							log.Println("error:", err)
							time.Sleep(time.Second * 10)
							continue
						}
						resp.Body.Close()
						requestsByStatusCode.WithLabelValues(h.String(), strconv.Itoa(resp.StatusCode)).Inc()
						if resp.StatusCode != *webhookStatusCode {
							setFailureMetrics(h.String(), "client_response")
							log.Println("error:", "Received response code", resp.StatusCode, ", expected", *webhookStatusCode)
							time.Sleep(time.Second * 10)
							continue
						}

						setSuccessMetrics(h.String(), begun)
						log.Println("successfully triggered reload")
						successfulReloadWebhook = true
						break
					}

					if !successfulReloadWebhook {
						setFailureMetrics(h.String(), "retries_exhausted")
						log.Println("error:", "Webhook reload retries exhausted")
					}
				}
			case err := <-watcher.Errors:
				watcherErrors.Inc()
				log.Println("error:", err)
			}
		}
	}()

	for _, d := range volumeDirs {
		log.Printf("Watching directory: %q", d)
		err = watcher.Add(d)
		if err != nil {
			log.Fatal(err)
		}
	}

	log.Fatal(serverMetrics(*listenAddress, *metricPath))
}

func envsubstFile(filePath string) error {

	if *subDestination == "" {
		log.Fatal("You must set a destinationpath for the substituted file")
	}

	dir, _ := filepath.Split(filePath)
	srcPath := filepath.Join(dir, *subFile)
	destPath := filepath.Join(*subDestination, *subFile)

	log.Printf("Substitute env vars in %s to destination %s", srcPath, destPath)

	fi, err := os.Lstat(srcPath)
	dat, err := ioutil.ReadFile(srcPath)
	if err != nil {
		log.Printf("Could not read file from fs event: %s, %s", srcPath, err)
		return err
	}

	buf, err := envsubst.Bytes(dat)
	if err != nil {
		log.Printf("error substituting env vars: %s\n", err)
		return err
	}

	err = os.WriteFile(destPath, buf, fi.Mode())
	if err != nil {
		log.Println("failed writing substituted file")
		return err
	}

	return nil
}

func setFailureMetrics(h, reason string) {
	requestErrorsByReason.WithLabelValues(h, reason).Inc()
	lastReloadError.WithLabelValues(h).Set(1.0)
}

func setSuccessMetrics(h string, begun time.Time) {
	requestDuration.WithLabelValues(h).Set(time.Since(begun).Seconds())
	successReloads.WithLabelValues(h).Inc()
	lastReloadError.WithLabelValues(h).Set(0.0)
}

func isValidEvent(event fsnotify.Event) bool {
	log.Printf("event: %s", event)
	if event.Op != fsnotify.Create {
		return false
	}

	if filepath.Base(event.Name) != "..data" {
		return false
	}
	return true
}

func serverMetrics(listenAddress, metricsPath string) error {
	http.Handle(metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`
			<html>
			<head><title>ConfigMap Reload Metrics</title></head>
			<body>
			<h1>ConfigMap Reload</h1>
			<p><a href='` + metricsPath + `'>Metrics</a></p>
			</body>
			</html>
		`))
	})
	return http.ListenAndServe(listenAddress, nil)
}

type volumeDirsFlag []string

type webhookFlag []*url.URL

func (v *volumeDirsFlag) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func (v *volumeDirsFlag) String() string {
	return fmt.Sprint(*v)
}

func (v *webhookFlag) Set(value string) error {
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	*v = append(*v, u)
	return nil
}

func (v *webhookFlag) String() string {
	return fmt.Sprint(*v)
}
