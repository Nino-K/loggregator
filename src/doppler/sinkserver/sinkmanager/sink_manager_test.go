package sinkmanager_test

import (
	"doppler/iprange"
	"doppler/sinks"
	"doppler/sinks/dump"
	"doppler/sinks/syslog"
	"doppler/sinks/syslogwriter"
	"doppler/sinkserver/blacklist"
	"doppler/sinkserver/metrics"
	"doppler/sinkserver/sinkmanager"
	"github.com/cloudfoundry/dropsonde/emitter"
	"github.com/cloudfoundry/dropsonde/events"
	"github.com/cloudfoundry/dropsonde/factories"
	"github.com/cloudfoundry/loggregatorlib/appservice"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent/instrumentation"
	"github.com/cloudfoundry/loggregatorlib/loggertesthelper"
	"github.com/gogo/protobuf/proto"
	"net/url"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("SinkManager", func() {
	var blackListManager = blacklist.New([]iprange.IPRange{iprange.IPRange{Start: "10.10.10.10", End: "10.10.10.20"}})
	var sinkManager *sinkmanager.SinkManager
	var sinkManagerDone chan struct{}
	var newAppServiceChan, deletedAppServiceChan chan appservice.AppService

	BeforeEach(func() {
		sinkManager = sinkmanager.NewSinkManager(1, true, blackListManager, loggertesthelper.Logger(), "dropsonde-origin", 1*time.Second, 1*time.Second)

		newAppServiceChan = make(chan appservice.AppService)
		deletedAppServiceChan = make(chan appservice.AppService)

		sinkManagerDone = make(chan struct{})
		go func() {
			defer close(sinkManagerDone)
			sinkManager.Start(newAppServiceChan, deletedAppServiceChan)
		}()
	})

	AfterEach(func() {
		sinkManager.Stop()
		<-sinkManagerDone
	})

	Describe("SendTo", func() {
		It("sends to all known sinks", func() {
			sink1 := &ChannelSink{appId: "myApp",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sink2 := &ChannelSink{appId: "myApp",
				identifier: "myAppChan2",
				done:       make(chan struct{}),
			}

			sinkManager.RegisterSink(sink1)
			sinkManager.RegisterSink(sink2)

			expectedMessageString := "Some Data"
			expectedMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedMessageString, "myApp", "App"), "origin")
			go sinkManager.SendTo("myApp", expectedMessage)

			Eventually(sink1.Received).Should(HaveLen(1))
			Eventually(sink2.Received).Should(HaveLen(1))
			Expect(sink1.Received()[0]).To(Equal(expectedMessage))
			Expect(sink2.Received()[0]).To(Equal(expectedMessage))
		})

		It("only sends to sinks that match the appID", func(done Done) {
			sink1 := &ChannelSink{appId: "myApp1",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sink2 := &ChannelSink{appId: "myApp2",
				identifier: "myAppChan2",
				done:       make(chan struct{}),
			}

			sinkManager.RegisterSink(sink1)
			sinkManager.RegisterSink(sink2)

			expectedMessageString := "Some Data"
			expectedMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedMessageString, "myApp", "App"), "origin")
			go sinkManager.SendTo("myApp1", expectedMessage)

			Eventually(sink1.Received).Should(HaveLen(1))
			Expect(sink1.Received()[0]).To(Equal(expectedMessage))
			Expect(sink2.Received()).To(BeEmpty())

			close(done)
		})

		It("sends messages to registered firehose sinks", func() {
			sink1 := &ChannelSink{done: make(chan struct{}), appId: "firehose-a"}
			sinkManager.RegisterFirehoseSink(sink1)

			expectedMessageString := "Some Data"
			expectedMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedMessageString, "myApp", "App"), "origin")
			go sinkManager.SendTo("myApp1", expectedMessage)

			Eventually(sink1.Received).Should(ContainElement(expectedMessage))
		})

		It("sends single message to app sink registered after the message was sent", func() {
			sink1 := &ChannelSink{appId: "myApp",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}

			expectedMessageString := "Some Data"
			expectedMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedMessageString, "myApp", "App"), "origin")
			go sinkManager.SendTo("myApp", expectedMessage)

			sinkManager.RegisterSink(sink1)

			Eventually(sink1.Received).Should(HaveLen(1))
			Expect(sink1.Received()[0]).To(Equal(expectedMessage))
		})

		It("sends single message to firehose sink registered after the message was sent", func() {
			sink1 := &ChannelSink{done: make(chan struct{}), appId: "firehose-a"}

			expectedMessageString := "Some Data"
			expectedMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedMessageString, "myApp", "App"), "origin")
			go sinkManager.SendTo("myApp1", expectedMessage)

			sinkManager.RegisterFirehoseSink(sink1)

			Eventually(sink1.Received).Should(ContainElement(expectedMessage))
		})

		Context("When a sync is consuming slowly", func() {
			It("buffers a reasonable number of messages", func() {
				ready := make(chan struct{})
				sink1 := &ChannelSink{appId: "myApp",
					identifier: "myAppChan1",
					done:       make(chan struct{}),
					ready:      ready,
				}

				sinkManager.RegisterSink(sink1)

				expectedFirstMessageString := "Some Data 1"
				expectedSecondMessageString := "Some Data 2"
				expectedFirstMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedFirstMessageString, "myApp", "App"), "origin")
				expectedSecondMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedSecondMessageString, "myApp", "App"), "origin")

				sinkManager.SendTo("myApp", expectedFirstMessage)
				sinkManager.SendTo("myApp", expectedSecondMessage)

				close(ready)

				Eventually(sink1.Received).Should(HaveLen(2))
				Expect(sink1.Received()[0]).To(Equal(expectedFirstMessage))
				Expect(sink1.Received()[1]).To(Equal(expectedSecondMessage))
			})
		})
	})

	Describe("Start", func() {
		Context("with updates from appstore", func() {
			var sinkManagerMetrics *metrics.SinkManagerMetrics
			var numSyslogSinks func() uint64

			BeforeEach(func() {
				sinkManagerMetrics = sinkManager.Metrics
				numSyslogSinks = func() uint64 {
					sinkManagerMetrics.RLock()
					defer sinkManagerMetrics.RUnlock()
					return sinkManagerMetrics.SyslogSinks
				}
			})

			Context("when an add update is received", func() {
				It("creates a new syslog sink with syslog writer from the newAppServicesChan", func() {
					initialNumSinks := numSyslogSinks()
					newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:885"}

					Eventually(numSyslogSinks).Should(Equal(uint64(initialNumSinks + 1)))
					key := metrics.AppDrainMetricKey{AppId: "aptastic", DrainURL: "syslog://127.0.1.1:885"}
					Expect(sinkManager.Metrics.AppDrainMetrics).Should(HaveKeyWithValue(key, uint64(0)))
				})

				It("creates a new syslog sink with tlsWriter from the newAppServicesChan", func() {
					initialNumSinks := numSyslogSinks()
					newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog-tls://127.0.1.1:885"}

					Eventually(numSyslogSinks).Should(Equal(uint64(initialNumSinks + 1)))
				})

				It("creates a new syslog sink with httpsWriter from the newAppServicesChan", func() {
					initialNumSinks := numSyslogSinks()
					newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "https://127.0.1.1:885"}

					Eventually(numSyslogSinks).Should(Equal(uint64(initialNumSinks + 1)))
				})

				Context("with an invalid drain Url", func() {
					var errorSink *ChannelSink

					BeforeEach(func() {
						errorSink = &ChannelSink{appId: "aptastic",
							identifier: "myAppChan1",
							done:       make(chan struct{}),
						}
						sinkManager.RegisterSink(errorSink)
					})

					It("sends an error message if the drain URL is blacklisted", func() {
						newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://10.10.10.11:884"}
						Eventually(errorSink.Received).Should(HaveLen(1))
						errorMsg := errorSink.Received()[0]
						Expect(string(errorMsg.GetLogMessage().GetMessage())).To(MatchRegexp("Invalid syslog drain URL"))
					})

					It("sends an error message if the drain URL is invalid", func() {
						newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog//invalid"}
						Eventually(errorSink.Received).Should(HaveLen(1))
						errorMsg := errorSink.Received()[0]
						Expect(string(errorMsg.GetLogMessage().GetMessage())).To(MatchRegexp("Invalid syslog drain URL"))
					})
				})
			})

			Context("when a delete update is received", func() {
				It("deletes the corresponding syslog sink if it exists", func() {
					initialNumSinks := numSyslogSinks()
					newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:886"}

					Eventually(numSyslogSinks).Should(Equal(uint64(initialNumSinks + 1)))
					key := metrics.AppDrainMetricKey{AppId: "aptastic", DrainURL: "syslog://127.0.1.1:886"}
					sinkManager.Metrics.AppDrainMetrics[key] = 0
					Expect(sinkManager.Metrics.AppDrainMetrics).To(HaveKey(key))

					deletedAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:886"}

					Eventually(numSyslogSinks).Should(Equal(uint64(initialNumSinks)))
					Expect(sinkManager.Metrics.AppDrainMetrics).ToNot(HaveKey(key))
				})

				It("handles a delete for a nonexistent sink", func() {
					initialNumSinks := numSyslogSinks()
					deletedAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:886"}
					Eventually(numSyslogSinks).Should(Equal(uint64(initialNumSinks)))
				})
			})
		})
	})

	Describe("Stop", func() {

		It("stops", func() {
			sinkManager.Stop()
			Eventually(sinkManagerDone).Should(BeClosed())
		})

		It("stops all registered sinks", func(done Done) {
			sink := &ChannelSink{appId: "myApp1",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sinkManager.RegisterSink(sink)
			sinkManager.Stop()
			Expect(sink.RunFinished()).To(BeTrue())

			close(done)
		})

		It("is idempotent", func() {
			sinkManager.Stop()
			Expect(sinkManager.Stop).NotTo(Panic())
		})
	})

	Describe("UnregisterSink", func() {
		Context("with a DumpSink", func() {
			var dumpSink *dump.DumpSink

			BeforeEach(func() {
				dumpSink = dump.NewDumpSink("appId", 1, loggertesthelper.Logger(), time.Hour, sinkManager.Metrics.AppDrainMetricsReceiverChan)
				sinkManager.RegisterSink(dumpSink)
			})

			It("clears the recent logs buffer", func() {
				expectedMessageString := "Some Data"
				expectedMessage, _ := emitter.Wrap(factories.NewLogMessage(events.LogMessage_OUT, expectedMessageString, "myApp", "App"), "origin")
				sinkManager.SendTo("appId", expectedMessage)

				Eventually(func() []*events.Envelope {
					return sinkManager.RecentLogsFor("appId")
				}).Should(HaveLen(1))

				sinkManager.UnregisterSink(dumpSink)

				Eventually(func() []*events.Envelope {
					return sinkManager.RecentLogsFor("appId")
				}).Should(HaveLen(0))
			})
		})

		Context("with a SyslogSink", func() {
			var syslogSink sinks.Sink

			BeforeEach(func() {
				url, err := url.Parse("syslog://localhost:9998")
				Expect(err).To(BeNil())
				writer, _ := syslogwriter.NewSyslogWriter(url, "appId")
				syslogSink = syslog.NewSyslogSink("appId", "localhost:9999", loggertesthelper.Logger(), writer, func(string, string, string) {}, "dropsonde-origin", sinkManager.Metrics.AppDrainMetricsReceiverChan)

				sinkManager.RegisterSink(syslogSink)
			})

			It("removes the sink", func() {
				Expect(sinkManager.Metrics.SyslogSinks).To(Equal(uint64(1)))

				sinkManager.UnregisterSink(syslogSink)

				Expect(sinkManager.Metrics.SyslogSinks).To(Equal(uint64(0)))
			})
		})

		Context("when called twice", func() {
			var dumpSink *dump.DumpSink

			BeforeEach(func() {
				dumpSink = dump.NewDumpSink("appId", 1, loggertesthelper.Logger(), time.Hour, sinkManager.Metrics.AppDrainMetricsReceiverChan)
				sinkManager.RegisterSink(dumpSink)
			})

			It("decrements the metric only once", func() {
				Expect(sinkManager.Metrics.DumpSinks).To(Equal(uint64(1)))
				sinkManager.UnregisterSink(dumpSink)
				Expect(sinkManager.Metrics.DumpSinks).To(Equal(uint64(0)))
				sinkManager.UnregisterSink(dumpSink)
				Expect(sinkManager.Metrics.DumpSinks).To(Equal(uint64(0)))
			})
		})
	})

	Describe("RegisterFirehoseSink", func() {
		It("runs the sink, updates metrics and returns true for registering a new firehose sink", func() {
			sink := &ChannelSink{done: make(chan struct{}), appId: "firehose-a"}
			Expect(sinkManager.RegisterFirehoseSink(sink)).To(BeTrue())
			Eventually(sink.RunCalled).Should(BeTrue())
			Expect(sinkManager.Metrics.Emit().Metrics[3].Value).To(Equal(uint64(1)))
		})

		It("returns false for a duplicate sink and does not update the sink metrics", func() {
			sink := &ChannelSink{done: make(chan struct{}), appId: "firehose-a"}

			Expect(sinkManager.RegisterFirehoseSink(sink)).To(BeTrue())

			Expect(sinkManager.RegisterFirehoseSink(sink)).To(BeFalse())
			Expect(sinkManager.Metrics.Emit().Metrics[3].Value).To(Equal(uint64(1)))
		})
	})

	Describe("UnregisterFirehoseSink", func() {
		It("stops the sink and updates metrics", func() {
			sink := &ChannelSink{done: make(chan struct{}), appId: "firehose-a"}

			Expect(sinkManager.RegisterFirehoseSink(sink)).To(BeTrue())

			sinkManager.UnregisterFirehoseSink(sink)
			Eventually(sink.RunFinished).Should(BeTrue())
			Expect(sinkManager.Metrics.Emit().Metrics[3].Value).To(Equal(uint64(0)))
		})

		It("does not update metrics when a sink is not registered", func() {
			sink := &ChannelSink{done: make(chan struct{})}

			sinkManager.UnregisterFirehoseSink(sink)
			Expect(sinkManager.Metrics.Emit().Metrics[3].Value).To(Equal(uint64(0)))
		})
	})

	Describe("Latest Container Metrics", func() {
		var sink *ChannelSink
		BeforeEach(func() {
			sink = &ChannelSink{
				appId:      "myApp",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sinkManager.RegisterSink(sink)
		})

		It("sends latest container metrics for a given app", func() {
			env := &events.Envelope{
				EventType: events.Envelope_ContainerMetric.Enum(),
				Timestamp: proto.Int64(time.Now().UnixNano()),
				ContainerMetric: &events.ContainerMetric{
					ApplicationId: proto.String("myApp"),
					InstanceIndex: proto.Int32(1),
					CpuPercentage: proto.Float64(73),
					MemoryBytes:   proto.Uint64(2),
					DiskBytes:     proto.Uint64(3),
				},
			}

			sinkManager.SendTo("myApp", env)

			Eventually(func() []*events.Envelope { return sinkManager.LatestContainerMetrics("myApp") }).Should(ConsistOf(env))
		})
	})

	Describe("SendSyslogErrorToLoggregator", func() {
		It("listens and broadcasts error messages", func() {
			sink := &ChannelSink{
				appId:      "myApp",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sinkManager.RegisterSink(sink)
			sinkManager.SendSyslogErrorToLoggregator("error msg", "myApp", "drainUrl")

			Eventually(sink.Received).Should(HaveLen(1))

			errorMsg := sink.Received()[0]
			Expect(string(errorMsg.GetLogMessage().GetMessage())).To(Equal("error msg"))
		})

		It("counts syslog send failures", func() {
			sink := &ChannelSink{
				appId:      "myApp",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sinkManager.RegisterSink(sink)
			sinkManager.SendSyslogErrorToLoggregator("error msg", "myApp", "drainUrl")

			syslogFailureMetrics := sinkManager.Metrics.Emit().Metrics[4:5]
			Expect(syslogFailureMetrics).To(ConsistOf(
				instrumentation.Metric{Name: "numberOfSyslogDrainErrors", Value: 1, Tags: map[string]interface{}{"appId": "myApp", "drainUrl": "drainUrl"}},
			))

		})
	})

	Describe("Emit", func() {
		It("emits all sink metrics", func() {
			sink1 := &ChannelSink{
				appId:                     "myApp1",
				identifier:                "myAppChan1",
				UpdateAppDrainMetricsChan: sinkManager.Metrics.AppDrainMetricsReceiverChan,
				done: make(chan struct{}),
			}
			sink2 := &ChannelSink{
				appId:                     "myApp2",
				identifier:                "myAppChan2",
				UpdateAppDrainMetricsChan: sinkManager.Metrics.AppDrainMetricsReceiverChan,
				done: make(chan struct{}),
			}
			sinkManager.RegisterSink(sink1)
			sinkManager.RegisterSink(sink2)
			sink1.UpdateAppDrainMetricsChan <- sinks.DrainMetric{AppId: "myApp1", DrainURL: "myAppChan1", DroppedMsgCount: uint64(50)}
			sink2.UpdateAppDrainMetricsChan <- sinks.DrainMetric{AppId: "myApp2", DrainURL: "myAppChan2", DroppedMsgCount: uint64(25)}

			var metrics []instrumentation.Metric

			Eventually(func() uint64 {
				metrics = sinkManager.Emit().Metrics
				return metrics[len(metrics)-1].Value.(uint64)
			}).Should(Equal(uint64(75)))

			appMetric := metrics[len(metrics)-3 : len(metrics)-1]
			Expect(appMetric).To(ConsistOf(
				instrumentation.Metric{Name: "numberOfMessagesLost", Value: uint64(50), Tags: map[string]interface{}{"appId": "myApp1", "drainUrl": "myAppChan1"}},
				instrumentation.Metric{Name: "numberOfMessagesLost", Value: uint64(25), Tags: map[string]interface{}{"appId": "myApp2", "drainUrl": "myAppChan2"}},
			))
		})
	})
})

type ChannelSink struct {
	sync.RWMutex
	done                      chan struct{}
	appId, identifier         string
	received                  []*events.Envelope
	runCalled                 bool
	ready                     chan struct{}
	UpdateAppDrainMetricsChan chan sinks.DrainMetric
}

func (c *ChannelSink) StreamId() string { return c.appId }
func (c *ChannelSink) Run(msgChan <-chan *events.Envelope) {
	if c.ready != nil {
		<-c.ready
	}
	c.Lock()
	c.runCalled = true
	c.Unlock()

	defer close(c.done)
	for msg := range msgChan {
		c.Lock()
		c.received = append(c.received, msg)
		c.Unlock()
	}
}

func (c *ChannelSink) RunFinished() bool {
	<-c.done
	return true
}

func (c *ChannelSink) RunCalled() bool {
	c.RLock()
	defer c.RUnlock()
	return c.runCalled
}

func (c *ChannelSink) Received() []*events.Envelope {
	c.RLock()
	defer c.RUnlock()
	data := make([]*events.Envelope, len(c.received))
	copy(data, c.received)
	return data
}

func (c *ChannelSink) Identifier() string        { return c.identifier }
func (c *ChannelSink) ShouldReceiveErrors() bool { return true }
func (c *ChannelSink) Emit() instrumentation.Context {
	return instrumentation.Context{}
}
func (c *ChannelSink) GetInstrumentationMetric() instrumentation.Metric {
	return instrumentation.Metric{Name: "numberOfMessagesLost", Tags: map[string]interface{}{"appId": string(c.appId)}, Value: 25}
}
func (c *ChannelSink) UpdateDroppedMessageCount(mc uint64) {}
