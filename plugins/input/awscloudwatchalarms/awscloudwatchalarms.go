package awscloudwatchalarms

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	internalaws "github.com/influxdata/telegraf/config/aws"
	"github.com/influxdata/telegraf/metric"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/filter"
)

type (
	// CloudWatch contains the configuration and cache for the cloudwatch plugin.
	CloudWatch struct {
		Region           string   `toml:"region"`
		AccessKey        string   `toml:"access_key"`
		SecretKey        string   `toml:"secret_key"`
		RoleARN          string   `toml:"role_arn"`
		Profile          string   `toml:"profile"`
		CredentialPath   string   `toml:"shared_credential_file"`
		Token            string   `toml:"token"`
		EndpointURL      string   `toml:"endpoint_url"`
		StatisticExclude []string `toml:"statistic_exclude"`
		StatisticInclude []string `toml:"statistic_include"`
		Namespace        string   `toml:"namespace"`
		RateLimit        int      `toml:"ratelimit"`

		Log telegraf.Logger `toml:"-"`

		client      cloudwatchClient
		statFilter  filter.Filter
		windowStart time.Time
		windowEnd   time.Time
	}
	cloudwatchClient interface {
		DescribeAlarms(*cloudwatch.DescribeAlarmsInput) (*cloudwatch.DescribeAlarmsOutput, error)
	}
)

// SampleConfig returns the default configuration of the Cloudwatch input plugin.
func (c *CloudWatch) SampleConfig() string {
	return `
  ## Amazon Region
  region = "us-east-1"

  ## Amazon Credentials
  ## Credentials are loaded in the following order
  ## 1) Assumed credentials via STS if role_arn is specified
  ## 2) explicit credentials from 'access_key' and 'secret_key'
  ## 3) shared profile from 'profile'
  ## 4) environment variables
  ## 5) shared credentials file
  ## 6) EC2 Instance Profile
  # access_key = ""
  # secret_key = ""
  # token = ""
  # role_arn = ""
  # profile = ""
  # shared_credential_file = ""

  ## Endpoint to make request against, the correct endpoint is automatically
  ## determined and this option should only be set if you wish to override the
  ## default.
  ##   ex: endpoint_url = "http://localhost:8000"
  # endpoint_url = ""



  ## Collection Delay (required - must account for metrics availability via CloudWatch API)
  delay = "5m"

  ## Recommended: use metric 'interval' that is a multiple of 'period' to avoid
  ## gaps or overlap in pulled data
  interval = "5m"

  ## Configure the TTL for the internal cache of metrics.
  # cache_ttl = "1h"

  ## Metric Statistic Namespace (required)
  namespace = "AWS/ELB"

  ## Maximum requests per second. Note that the global default AWS rate limit is
  ## 50 reqs/sec, so if you define multiple namespaces, these should add up to a
  ## maximum of 50.
  ## See http://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/cloudwatch_limits.html
  # ratelimit = 25

  ## Timeout for http requests made by the cloudwatch client.
  # timeout = "5s"

  ## Namespace-wide statistic filters. These allow fewer queries to be made to
  ## cloudwatch.
  # statistic_include = [ "average", "sum", "minimum", "maximum", sample_count" ]
  # statistic_exclude = []

  ## Metrics to Pull
  ## Defaults to all Metrics in Namespace if nothing is provided
  ## Refreshes Namespace available metrics every 1h
  #[[inputs.cloudwatch.metrics]]
  #  names = ["Latency", "RequestCount"]
  #
  #  ## Statistic filters for Metric.  These allow for retrieving specific
  #  ## statistics for an individual metric.
  #  # statistic_include = [ "average", "sum", "minimum", "maximum", sample_count" ]
  #  # statistic_exclude = []
  #
  #  ## Dimension filters for Metric.  All dimensions defined for the metric names
  #  ## must be specified in order to retrieve the metric statistics.
  #  [[inputs.cloudwatch.metrics.dimensions]]
  #    name = "LoadBalancerName"
  #    value = "p-example"
`
}

// Description returns a one-sentence description on the Cloudwatch input plugin.
func (c *CloudWatch) Description() string {
	return "Pull Metric Statistics from Amazon CloudWatch"
}

// Gather takes in an accumulator and adds the metrics that the Input
// gathers. This is called every "interval".
func (c *CloudWatch) Gather(acc telegraf.Accumulator) error {
	if c.statFilter == nil {
		var err error
		// Set config level filter (won't change throughout life of plugin).
		c.statFilter, err = filter.NewIncludeExcludeFilter(c.StatisticInclude, c.StatisticExclude)
		if err != nil {
			return err
		}
	}

	if c.client == nil {
		c.initializeCloudWatch()
	}

	wg := sync.WaitGroup{}
	rLock := sync.Mutex{}

	alarms := []*cloudwatch.MetricAlarm{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err := c.client.DescribeAlarms(nil)
		if err != nil {
			acc.AddError(err)
			return
		}

		rLock.Lock()
		alarms = result.MetricAlarms
		rLock.Unlock()
	}()
	wg.Wait()

	return c.aggregateAlarms(acc, alarms)

}

func (c *CloudWatch) aggregateAlarms(
	acc telegraf.Accumulator,
	metricAlarmResults []*cloudwatch.MetricAlarm,
) error {
	var (
		grouper = metric.NewSeriesGrouper()
	)

	for _, result := range metricAlarmResults {
		tags := map[string]string{}
		tags["region"] = c.Region
		tags["alarmArn"] = *result.AlarmArn
		tags["metricName"] = *result.MetricName
		tags["namespace"] = *result.Namespace
		for i := range result.Dimensions {
			tags[*result.Dimensions[i].Name] = *result.Dimensions[i].Value
		}
		fmt.Println(*result.AlarmName)
		grouper.Add(*result.AlarmName, tags, *result.StateUpdatedTimestamp, "State", *result.StateValue)

	}

	for _, metric := range grouper.Metrics() {
		acc.AddMetric(metric)
	}

	return nil
}

func (c *CloudWatch) getAlarms(params *cloudwatch.DescribeAlarmsInput) ([]*cloudwatch.MetricAlarm, error) {

	results := []*cloudwatch.MetricAlarm{}

	for {
		resp, err := c.client.DescribeAlarms(params)
		if err != nil {
			return nil, fmt.Errorf("failed to get Alarm data: %v", err)
		}

		results = append(results, resp.MetricAlarms...)
		if resp.NextToken == nil {
			break
		}
		params.NextToken = resp.NextToken
	}
	return results, nil
}

func (c *CloudWatch) initializeCloudWatch() {
	credentialConfig := &internalaws.CredentialConfig{
		Region:      c.Region,
		AccessKey:   c.AccessKey,
		SecretKey:   c.SecretKey,
		RoleARN:     c.RoleARN,
		Profile:     c.Profile,
		Filename:    c.CredentialPath,
		Token:       c.Token,
		EndpointURL: c.EndpointURL,
	}
	configProvider := credentialConfig.Credentials()

	cfg := &aws.Config{
		HTTPClient: &http.Client{
			// use values from DefaultTransport
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
					DualStack: true,
				}).DialContext,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			Timeout: 60 * time.Second,
		},
	}

	loglevel := aws.LogOff
	c.client = cloudwatch.New(configProvider, cfg.WithLogLevel(loglevel))

}
