/*
 * Copyright NetApp Inc, 2021 All rights reserved

Package Description:

   The Prometheus exporter exposes metrics to the Prometheus DB
   over an HTTP server. It consists of two concurrent components:

      - the "actual" exporter (this file): receives metrics from collectors,
        renders into the Prometheus format and stores in cache

      - the HTTP daemon (httpd.go): will listen for incoming requests and
        will serve metrics from that cache.

   Strictly speaking this is an HTTP-exporter, simply using the exposition
   format accepted by Prometheus.

   Special thanks Yann Bizeul who helped to identify that having no lock
   on the cache creates a race-condition (not caught on all Linux systems).
*/

package prometheus

import (
	"fmt"
	"github.com/netapp/harvest/v2/cmd/poller/exporter"
	"github.com/netapp/harvest/v2/cmd/poller/plugin/changelog"
	"github.com/netapp/harvest/v2/pkg/errs"
	"github.com/netapp/harvest/v2/pkg/matrix"
	"github.com/netapp/harvest/v2/pkg/set"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Default parameters
const (
	// the maximum amount of time to keep metrics in the cache
	cacheMaxKeep = "5m"
	// apply a prefix to metrics globally (default none)
	globalPrefix = ""
)

type Prometheus struct {
	*exporter.AbstractExporter
	cache           *cache
	allowAddrs      []string
	allowAddrsRegex []*regexp.Regexp
	cacheAddrs      map[string]bool
	checkAddrs      bool
	addMetaTags     bool
	globalPrefix    string
	replacer        *strings.Replacer
}

func New(abc *exporter.AbstractExporter) exporter.Exporter {
	return &Prometheus{AbstractExporter: abc}
}

func (p *Prometheus) Init() error {

	if err := p.InitAbc(); err != nil {
		return err
	}

	// from abstract class, we get "export" and "render" time
	// some additional metadata instances
	if instance, err := p.Metadata.NewInstance("http"); err == nil {
		instance.SetLabel("task", "http")
	} else {
		return err
	}

	p.replacer = newReplacer()

	if instance, err := p.Metadata.NewInstance("info"); err == nil {
		instance.SetLabel("task", "info")
	} else {
		return err
	}

	if x := p.Params.GlobalPrefix; x != nil {
		p.Logger.Debug().Msgf("will use global prefix [%s]", *x)
		p.globalPrefix = *x
		if !strings.HasSuffix(p.globalPrefix, "_") {
			p.globalPrefix += "_"
		}
	} else {
		p.globalPrefix = globalPrefix
	}

	// add HELP and TYPE tags to exported metrics if requested
	if p.Params.ShouldAddMetaTags != nil && *p.Params.ShouldAddMetaTags {
		p.addMetaTags = true
	}

	// all other parameters are only relevant to the HTTP daemon
	if x := p.Params.CacheMaxKeep; x != nil {
		if d, err := time.ParseDuration(*x); err == nil {
			p.Logger.Debug().Msgf("using cache_max_keep [%s]", *x)
			p.cache = newCache(d)
		} else {
			p.Logger.Error().Stack().Err(err).Msgf("cache_max_keep [%s]", *x)
		}
	}

	if p.cache == nil {
		p.Logger.Debug().Msgf("using default cache_max_keep [%s]", cacheMaxKeep)
		if d, err := time.ParseDuration(cacheMaxKeep); err == nil {
			p.cache = newCache(d)
		} else {
			return err
		}
	}

	// allow access to metrics only from the given plain addresses
	if x := p.Params.AllowedAddrs; x != nil {
		p.allowAddrs = *x
		if len(p.allowAddrs) == 0 {
			p.Logger.Error().Stack().Err(nil).Msg("allow_addrs without any")
			return errs.New(errs.ErrInvalidParam, "allow_addrs")
		}
		p.checkAddrs = true
		p.Logger.Debug().Msgf("added %d plain allow rules", len(p.allowAddrs))
	}

	// allow access only from addresses matching one of defined regular expressions
	if x := p.Params.AllowedAddrsRegex; x != nil {
		p.allowAddrsRegex = make([]*regexp.Regexp, 0)
		for _, r := range *x {
			r = strings.TrimPrefix(strings.TrimSuffix(r, "`"), "`")
			if reg, err := regexp.Compile(r); err == nil {
				p.allowAddrsRegex = append(p.allowAddrsRegex, reg)
			} else {
				p.Logger.Error().Stack().Err(err).Msg("parse regex")
				return errs.New(errs.ErrInvalidParam, "allow_addrs_regex")
			}
		}
		if len(p.allowAddrsRegex) == 0 {
			p.Logger.Error().Stack().Err(nil).Msg("allow_addrs_regex without any")
			return errs.New(errs.ErrInvalidParam, "allow_addrs")
		}
		p.checkAddrs = true
		p.Logger.Debug().Msgf("added %d regex allow rules", len(p.allowAddrsRegex))
	}

	// cache addresses that have been allowed or denied already
	if p.checkAddrs {
		p.cacheAddrs = make(map[string]bool)
	}

	// Finally, the most important and only required parameter: port
	// can be passed to us either as an option or as a parameter
	port := p.Options.PromPort
	if port == 0 {
		if promPort := p.Params.Port; promPort == nil {
			p.Logger.Error().Stack().Err(nil).Msg("Issue while reading prometheus port")
		} else {
			port = *promPort
		}
	}

	// Make sure port is valid
	if port == 0 {
		return errs.New(errs.ErrMissingParam, "port")
	} else if port < 0 {
		return errs.New(errs.ErrInvalidParam, "port")
	}

	// The optional parameter LocalHTTPAddr is the address of the HTTP service, valid values are:
	// - "localhost" or "127.0.0.1", this limits access to local machine
	// - "" (default) or "0.0.0.0", allows access from network
	addr := p.Params.LocalHTTPAddr
	if addr != "" {
		p.Logger.Debug().Str("addr", addr).Msg("Using custom local addr")
	}

	if !p.Params.IsTest {
		go p.startHTTPD(addr, port)
	}

	// @TODO: implement error checking to enter failed state if HTTPd failed
	// (like we did in Alpha)

	//goland:noinspection HttpUrlsUsage
	p.Logger.Debug().Msgf("initialized, HTTP daemon started at [http://%s:%d]", addr, port)

	return nil
}

func newReplacer() *strings.Replacer {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", "\\n")
}

// Export - Unlike other Harvest exporters, we don't export data
// but put it in cache. The HTTP daemon serves that cache on request.
//
// An important aspect of the whole mechanism is that all incoming
// data should have a unique UUID and object pair, otherwise they'll
// overwrite other data in the cache.
// This key is also used by the HTTP daemon to trace back the name
// of the collectors and plugins where the metrics come from (for the info page)
func (p *Prometheus) Export(data *matrix.Matrix) (exporter.Stats, error) {

	var (
		metrics [][]byte
		stats   exporter.Stats
		err     error
	)

	// lock the exporter, to prevent other collectors from writing to us
	p.Lock()
	defer p.Unlock()

	// render metrics into Prometheus format
	start := time.Now()
	metrics, stats = p.render(data)

	// fix render time for metadata
	d := time.Since(start)

	// store metrics in cache
	key := data.UUID + "." + data.Object + "." + data.Identifier

	// lock cache, to prevent HTTPd reading while we are mutating it
	p.cache.Lock()
	p.cache.Put(key, metrics)
	p.cache.Unlock()

	// update metadata
	p.AddExportCount(uint64(len(metrics)))
	err = p.Metadata.LazyAddValueInt64("time", "render", d.Microseconds())
	if err != nil {
		p.Logger.Error().Err(err).Msg("error")
	}
	err = p.Metadata.LazyAddValueInt64("time", "export", time.Since(start).Microseconds())
	if err != nil {
		p.Logger.Error().Err(err).Msg("error")
	}

	return stats, nil
}

// Render metrics and labels into the exposition format, as described in
// https://prometheus.io/docs/instrumenting/exposition_formats/
//
// All metrics are implicitly "Gauge" counters. If requested, we also submit
// HELP and TYPE metadata (see add_meta_tags in config).
//
// Metric name is concatenation of the collector object (e.g. "volume",
// "fcp_lif") + the metric name (e.g. "read_ops" => "volume_read_ops").
// We do this since the same metrics for different objects can have
// different sets of labels, and Prometheus does not allow this.
//
// Example outputs:
//
// volume_read_ops{node="my-node",vol="some_vol"} 2523
// fcp_lif_read_ops{vserver="nas_svm",port_id="e02"} 771

func (p *Prometheus) render(data *matrix.Matrix) ([][]byte, exporter.Stats) {
	var (
		rendered          [][]byte
		tagged            *set.Set
		labelsToInclude   []string
		keysToInclude     []string
		prefix            string
		err               error
		histograms        map[string]*histogram
		normalizedLabels  map[string][]string // cache of histogram normalized labels
		instancesExported uint64
	)

	rendered = make([][]byte, 0)
	globalLabels := make([]string, 0, len(data.GetGlobalLabels()))
	normalizedLabels = make(map[string][]string)

	if p.addMetaTags {
		tagged = set.New()
	}

	options := data.GetExportOptions()

	if x := options.GetChildS("instance_labels"); x != nil {
		labelsToInclude = x.GetAllChildContentS()
	}

	if x := options.GetChildS("instance_keys"); x != nil {
		keysToInclude = x.GetAllChildContentS()
	}

	includeAllLabels := false
	requireInstanceKeys := true

	if x := options.GetChildContentS("include_all_labels"); x != "" {
		if includeAllLabels, err = strconv.ParseBool(x); err != nil {
			p.Logger.Error().Stack().Err(err).Msg("parameter: include_all_labels")
		}
	}

	if x := options.GetChildContentS("require_instance_keys"); x != "" {
		if requireInstanceKeys, err = strconv.ParseBool(x); err != nil {
			p.Logger.Error().Stack().Err(err).Msg("parameter: require_instance_keys")
		}
	}

	prefix = p.globalPrefix + data.Object

	for key, value := range data.GetGlobalLabels() {
		globalLabels = append(globalLabels, escape(p.replacer, key, value))
	}

	for _, instance := range data.GetInstances() {

		if !instance.IsExportable() {
			continue
		}
		instancesExported++

		instanceKeys := make([]string, len(globalLabels))
		copy(instanceKeys, globalLabels)
		instanceKeysOk := false
		instanceLabels := make([]string, 0)
		instanceLabelsSet := make(map[string]struct{})

		// The ChangeLog plugin tracks metric values and publishes the names of metrics that have changed.
		// For example, it might indicate that 'volume_size_total' has been updated.
		// If a global prefix for the exporter is defined, we need to amend the metric name with this prefix.
		if p.globalPrefix != "" && data.Object == changelog.ObjectChangeLog {
			if categoryValue, ok := instance.GetLabels()[changelog.Category]; ok {
				if categoryValue == changelog.Metric {
					if tracked, ok := instance.GetLabels()[changelog.Track]; ok {
						instance.GetLabels()[changelog.Track] = p.globalPrefix + tracked
					}
				}
			}
		}

		if includeAllLabels {
			for label, value := range instance.GetLabels() {
				// temporary fix for the rarely happening duplicate labels
				// known case is: ZapiPerf -> 7mode -> disk.yaml
				// actual cause is the Aggregator plugin, which is adding node as
				// instance label (even though it's already a global label for 7modes)
				_, ok := data.GetGlobalLabels()[label]
				if !ok {
					instanceKeys = append(instanceKeys, escape(p.replacer, label, value)) //nolint:makezero
				}
			}
		} else {
			for _, key := range keysToInclude {
				value := instance.GetLabel(key)
				instanceKeys = append(instanceKeys, escape(p.replacer, key, value)) //nolint:makezero
				if !instanceKeysOk && value != "" {
					instanceKeysOk = true
				}
			}

			for _, label := range labelsToInclude {
				value := instance.GetLabel(label)
				kv := escape(p.replacer, label, value)
				_, ok := instanceLabelsSet[kv]
				if ok {
					continue
				}
				instanceLabelsSet[kv] = struct{}{}
				instanceLabels = append(instanceLabels, kv)
			}

			// @TODO, probably be strict, and require all keys to be present
			if !instanceKeysOk && requireInstanceKeys {
				continue
			}

			// @TODO, check at least one label is found?
			if len(instanceLabels) != 0 {
				allLabels := make([]string, len(instanceLabels))
				copy(allLabels, instanceLabels)
				// include each instanceKey not already included in the list of labels
				for _, instanceKey := range instanceKeys {
					_, ok := instanceLabelsSet[instanceKey]
					if ok {
						continue
					}
					instanceLabelsSet[instanceKey] = struct{}{}
					allLabels = append(allLabels, instanceKey) //nolint:makezero
				}
				if p.Params.SortLabels {
					sort.Strings(allLabels)
				}
				labelData := fmt.Sprintf("%s_labels{%s} 1.0", prefix, strings.Join(allLabels, ","))

				if tagged != nil && !tagged.Has(prefix+"_labels") {
					tagged.Add(prefix + "_labels")
					rendered = append(rendered,
						[]byte("# HELP "+prefix+"_labels Pseudo-metric for "+data.Object+" labels"),
						[]byte("# TYPE "+prefix+"_labels gauge"))
				}
				rendered = append(rendered, []byte(labelData))
			}
		}

		if p.Params.SortLabels {
			sort.Strings(instanceKeys)
		}
		histograms = make(map[string]*histogram)
		for _, metric := range data.GetMetrics() {

			if !metric.IsExportable() {
				continue
			}

			if value, ok := metric.GetValueString(instance); ok {

				// metric is array, determine if this is a plain array or histogram
				if metric.HasLabels() {
					if metric.IsHistogram() {
						// Metric is histogram. Create a new metric to accumulate
						// the flattened metrics and export them in order
						bucketMetric := data.GetMetric(metric.GetLabel("bucket"))
						if bucketMetric == nil {
							p.Logger.Debug().
								Str("metric", metric.GetName()).
								Msg("Unable to find bucket for metric, skip")
							continue
						}
						metricIndex := metric.GetLabel("comment")
						index, err := strconv.Atoi(metricIndex)
						if err != nil {
							p.Logger.Error().Err(err).
								Str("metric", metric.GetName()).
								Str("index", metricIndex).
								Msg("Unable to find index of metric, skip")
						}
						histogram := histogramFromBucket(histograms, bucketMetric)
						histogram.values[index] = value
						continue
					}
					metricLabels := make([]string, 0, len(metric.GetLabels()))
					for k, v := range metric.GetLabels() {
						metricLabels = append(metricLabels, escape(p.replacer, k, v))
					}
					x := fmt.Sprintf(
						"%s_%s{%s,%s} %s",
						prefix,
						metric.GetName(),
						strings.Join(instanceKeys, ","),
						strings.Join(metricLabels, ","),
						value,
					)

					if tagged != nil && !tagged.Has(prefix+"_"+metric.GetName()) {
						tagged.Add(prefix + "_" + metric.GetName())
						rendered = append(rendered,
							[]byte("# HELP "+prefix+"_"+metric.GetName()+" Metric for "+data.Object),
							[]byte("# TYPE "+prefix+"_"+metric.GetName()+" histogram"))
					}

					rendered = append(rendered, []byte(x))
					// scalar metric
				} else {
					x := metric.GetName() + "{" + strings.Join(instanceKeys, ",") + "} " + value
					if prefix != "" {
						x = prefix + "_" + x
					}

					if tagged != nil && !tagged.Has(prefix+"_"+metric.GetName()) {
						tagged.Add(prefix + "_" + metric.GetName())
						rendered = append(rendered,
							[]byte("# HELP "+prefix+"_"+metric.GetName()+" Metric for "+data.Object),
							[]byte("# TYPE "+prefix+"_"+metric.GetName()+" gauge"))
					}

					rendered = append(rendered, []byte(x))
				}
			}
		}
		// All metrics have been processed and flattened metrics accumulated. Determine which histograms can be
		// normalized and export
		for _, h := range histograms {
			metric := h.metric
			bucketNames := metric.Buckets()
			objectMetric := data.Object + "_" + metric.GetName()
			_, ok := normalizedLabels[objectMetric]
			if !ok {
				canNormalize := true
				normalizedNames := make([]string, 0, len(*bucketNames))
				// check if the buckets can be normalized and collect normalized names
				for _, bucketName := range *bucketNames {
					normalized := p.normalizeHistogram(bucketName)
					if normalized == "" {
						canNormalize = false
						break
					}
					normalizedNames = append(normalizedNames, normalized)
				}
				if canNormalize {
					normalizedLabels[objectMetric] = normalizedNames
				}
			}

			if tagged != nil && !tagged.Has(prefix+"_"+metric.GetName()) {
				tagged.Add(prefix + "_" + metric.GetName())
				rendered = append(rendered,
					[]byte("# HELP "+prefix+"_"+metric.GetName()+" Metric for "+data.Object),
					[]byte("# TYPE "+prefix+"_"+metric.GetName()+" histogram"))
			}

			normalizedNames, canNormalize := normalizedLabels[objectMetric]
			var (
				countMetric string
				sumMetric   string
			)
			if canNormalize {
				count, sum := h.computeCountAndSum(normalizedNames)
				countMetric = fmt.Sprintf("%s_%s{%s} %s",
					prefix, metric.GetName()+"_count", strings.Join(instanceKeys, ","), count)
				sumMetric = fmt.Sprintf("%s_%s{%s} %d",
					prefix, metric.GetName()+"_sum", strings.Join(instanceKeys, ","), sum)
			}
			for i, value := range h.values {
				bucketName := (*bucketNames)[i]
				var x string
				if canNormalize {
					x = fmt.Sprintf(
						"%s_%s{%s,%s} %s",
						prefix,
						metric.GetName()+"_bucket",
						strings.Join(instanceKeys, ","),
						`le="`+normalizedNames[i]+`"`,
						value,
					)
				} else {
					x = fmt.Sprintf(
						"%s_%s{%s,%s} %s",
						prefix,
						metric.GetName(),
						strings.Join(instanceKeys, ","),
						escape(p.replacer, "metric", bucketName),
						value,
					)
				}
				rendered = append(rendered, []byte(x))
			}
			if canNormalize {
				rendered = append(rendered, []byte(countMetric), []byte(sumMetric))
			}
		}
	}
	stats := exporter.Stats{
		InstancesExported: instancesExported,
		MetricsExported:   uint64(len(rendered)),
	}

	return rendered, stats
}

var numAndUnitRe = regexp.MustCompile(`(\d+)\s*(\w+)`)

// normalizeHistogram tries to normalize ONTAP values by converting units to multiples of the smallest unit.
// When the unit cannot be determined, return an empty string
func (p *Prometheus) normalizeHistogram(ontap string) string {
	numAndUnit := ontap
	if strings.HasPrefix(ontap, "<") {
		numAndUnit = ontap[1:]
	} else if strings.HasPrefix(ontap, ">") {
		return "+Inf"
	}
	submatch := numAndUnitRe.FindStringSubmatch(numAndUnit)
	if len(submatch) != 3 {
		return ""
	}
	num := submatch[1]
	unit := submatch[2]
	float, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return ""
	}
	var normal float64
	switch unit {
	case "us":
		return num
	case "ms", "msec":
		normal = 1_000 * float
	case "s", "sec":
		normal = 1_000_000 * float
	default:
		return ""
	}
	return strconv.FormatFloat(normal, 'f', -1, 64)
}

func histogramFromBucket(histograms map[string]*histogram, metric *matrix.Metric) *histogram {
	h, ok := histograms[metric.GetName()]
	if ok {
		return h
	}
	buckets := metric.Buckets()
	var capacity int
	if buckets != nil {
		capacity = len(*buckets)
	}
	h = &histogram{
		metric: metric,
		values: make([]string, capacity),
	}
	histograms[metric.GetName()] = h
	return h
}

func escape(replacer *strings.Replacer, key string, value string) string {
	// See https://prometheus.io/docs/instrumenting/exposition_formats/#comments-help-text-and-type-information
	// label_value can be any sequence of UTF-8 characters, but the backslash (\), double-quote ("),
	// and line feed (\n) characters have to be escaped as \\, \", and \n, respectively.

	return key + "=" + strconv.Quote(replacer.Replace(value))
}

type histogram struct {
	metric *matrix.Metric
	values []string
}

func (h *histogram) computeCountAndSum(normalizedNames []string) (string, int) {
	// If the buckets are normalizable, iterate through the values to:
	// 1) calculate Prometheus's cumulative buckets
	// 2) add _count metric
	// 3) calculate and add _sum metric
	cumValues := make([]string, len(h.values))
	runningTotal := 0
	sum := 0
	for i, value := range h.values {
		num, _ := strconv.Atoi(value)
		runningTotal += num
		cumValues[i] = strconv.Itoa(runningTotal)
		normalName := normalizedNames[i]
		leValue, _ := strconv.Atoi(normalName)
		sum += leValue * num
	}
	h.values = cumValues
	return cumValues[len(cumValues)-1], sum
}
