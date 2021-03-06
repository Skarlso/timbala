package fanout

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/golang/snappy"
	"github.com/mattbostock/timbala/internal/cluster"
	"github.com/mattbostock/timbala/internal/read"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context/ctxhttp"
)

const readTimeoutSeconds = 30 * time.Second

var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			DualStack: true,
			KeepAlive: 10 * time.Minute,
			Timeout:   2 * time.Second,
		}).DialContext,
		ExpectContinueTimeout: 5 * time.Second,
		IdleConnTimeout:       10 * time.Minute,
		ResponseHeaderTimeout: 5 * time.Second,
	}}

type fanoutStorage struct {
	clstr      cluster.Cluster
	localStore storage.Storage
	log        *logrus.Logger
}

func New(c cluster.Cluster, l *logrus.Logger, s storage.Storage) *fanoutStorage {
	return &fanoutStorage{
		clstr:      c,
		localStore: s,
		log:        l,
	}
}

func (f *fanoutStorage) Querier(ctx context.Context, mint int64, maxt int64) (storage.Querier, error) {
	localQuerier, err := f.localStore.Querier(ctx, mint, maxt)
	if err != nil {
		return nil, err
	}
	queriers := append([]storage.Querier{}, localQuerier)

	// FIXME handle cluster node membership changes
	for _, n := range f.clstr.Nodes() {
		if n.Name() == f.clstr.LocalNode().Name() {
			continue
		}

		httpAddr, err := n.HTTPAddr()
		if err != nil {
			return nil, err
		}

		queriers = append(queriers, remoteQuerier{
			ctx:  ctx,
			maxt: maxt,
			mint: mint,
			// FIXME handle HTTPS
			url: "http://" + httpAddr + read.Route,
		})
	}

	return storage.NewMergeQuerier(queriers), nil
}

func (f *fanoutStorage) Appender() (storage.Appender, error) {
	panic("not implemented")
}

func (f *fanoutStorage) StartTime() (int64, error) {
	return 0, nil
}

func (f *fanoutStorage) Close() error {
	return nil
}

type remoteQuerier struct {
	ctx        context.Context
	mint, maxt int64
	url        string
}

func (q remoteQuerier) Select(matchers ...*labels.Matcher) (storage.SeriesSet, error) {
	protoMatchers, err := toLabelMatchers(matchers)
	if err != nil {
		return nil, err
	}

	res, err := q.remoteRead(protoMatchers)
	// FIXME: Don't fail if just one node fails to respond
	if err != nil {
		return nil, err
	}

	series := make([]storage.Series, 0, len(res))
	for _, ts := range res {
		labels := labelPairsToLabels(ts.Labels)
		series = append(series, &concreteSeries{
			labels:  labels,
			samples: ts.Samples,
		})
	}
	sort.Sort(byLabel(series))
	return &concreteSeriesSet{
		series: series,
	}, nil
}

func (_ remoteQuerier) LabelValues(name string) ([]string, error) {
	panic("not implemented")
}

func (_ remoteQuerier) Close() error {
	// Nothing to do
	return nil
}

func (q remoteQuerier) remoteRead(matchers []*prompb.LabelMatcher) ([]*prompb.TimeSeries, error) {
	req := &prompb.ReadRequest{
		// FIXME: Support batching multiple queries into one read
		// request, as the protobuf interface allows for it.
		Queries: []*prompb.Query{{
			StartTimestampMs: q.mint,
			EndTimestampMs:   q.maxt,
			Matchers:         matchers,
		}},
	}

	data, err := req.Marshal()
	if err != nil {
		return nil, fmt.Errorf("unable to marshal read request: %v", err)
	}

	compressed := snappy.Encode(nil, data)
	httpReq, err := http.NewRequest("POST", q.url, bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("unable to create request: %v", err)
	}
	httpReq.Header.Add("Content-Encoding", "snappy")
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set(read.HTTPHeaderRemoteRead, read.HTTPHeaderRemoteReadVersion)
	httpReq.Header.Set(read.HTTPHeaderInternalRead, read.HTTPHeaderInternalReadVersion)

	ctx, cancel := context.WithTimeout(q.ctx, readTimeoutSeconds)
	defer cancel()

	httpResp, err := ctxhttp.Do(ctx, httpClient, httpReq)
	if err != nil {
		return nil, fmt.Errorf("error sending request: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("server returned HTTP status %s", httpResp.Status)
	}

	compressed, err = ioutil.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %v", err)
	}

	uncompressed, err := snappy.Decode(nil, compressed)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %v", err)
	}

	var resp prompb.ReadResponse
	err = resp.Unmarshal(uncompressed)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal response body: %v", err)
	}

	if len(resp.Results) != len(req.Queries) {
		return nil, fmt.Errorf("responses: want %d, got %d", len(req.Queries), len(resp.Results))
	}

	return resp.Results[0].Timeseries, nil
}

func labelPairsToLabels(labelPairs []*prompb.Label) labels.Labels {
	result := make(labels.Labels, 0, len(labelPairs))
	for _, l := range labelPairs {
		result = append(result, labels.Label{
			Name:  l.Name,
			Value: l.Value,
		})
	}
	sort.Sort(result)
	return result
}

// concreteSeriesSet implements storage.SeriesSet.
type concreteSeriesSet struct {
	cur    int
	series []storage.Series
}

func (c *concreteSeriesSet) Next() bool {
	c.cur++
	return c.cur-1 < len(c.series)
}

func (c *concreteSeriesSet) At() storage.Series {
	return c.series[c.cur-1]
}

func (c *concreteSeriesSet) Err() error {
	return nil
}

// concreteSeries implementes storage.Series.
type concreteSeries struct {
	labels  labels.Labels
	samples []*prompb.Sample
}

func (c *concreteSeries) Labels() labels.Labels {
	return c.labels
}

func (c *concreteSeries) Iterator() storage.SeriesIterator {
	return newConcreteSeriersIterator(c)
}

// concreteSeriesIterator implements storage.SeriesIterator.
type concreteSeriesIterator struct {
	cur    int
	series *concreteSeries
}

func newConcreteSeriersIterator(series *concreteSeries) storage.SeriesIterator {
	return &concreteSeriesIterator{
		cur:    -1,
		series: series,
	}
}

func (c *concreteSeriesIterator) Seek(t int64) bool {
	c.cur = sort.Search(len(c.series.samples), func(n int) bool {
		return c.series.samples[n].Timestamp >= t
	})
	return c.cur < len(c.series.samples)
}

func (c *concreteSeriesIterator) At() (t int64, v float64) {
	s := c.series.samples[c.cur]
	return s.Timestamp, s.Value
}

func (c *concreteSeriesIterator) Next() bool {
	c.cur++
	return c.cur < len(c.series.samples)
}

func (c *concreteSeriesIterator) Err() error {
	return nil
}

type byLabel []storage.Series

func (a byLabel) Len() int           { return len(a) }
func (a byLabel) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byLabel) Less(i, j int) bool { return labels.Compare(a[i].Labels(), a[j].Labels()) < 0 }

// FIXME: Deduplicate this code copied from the read package (copied from Prometheus)
func toLabelMatchers(matchers []*labels.Matcher) ([]*prompb.LabelMatcher, error) {
	result := make([]*prompb.LabelMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		var mType prompb.LabelMatcher_Type
		switch matcher.Type {
		case labels.MatchEqual:
			mType = prompb.LabelMatcher_EQ
		case labels.MatchNotEqual:
			mType = prompb.LabelMatcher_NEQ
		case labels.MatchRegexp:
			mType = prompb.LabelMatcher_RE
		case labels.MatchNotRegexp:
			mType = prompb.LabelMatcher_NRE
		default:
			return nil, fmt.Errorf("invalid matcher type")
		}
		result = append(result, &prompb.LabelMatcher{
			Type:  mType,
			Name:  string(matcher.Name),
			Value: string(matcher.Value),
		})
	}
	return result, nil
}
