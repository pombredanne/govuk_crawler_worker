package main_test

import (
	"time"

	. "github.com/alphagov/govuk_crawler_worker"
	. "github.com/alphagov/govuk_crawler_worker/queue"
	. "github.com/alphagov/govuk_crawler_worker/ttl_hash_set"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/fzzy/radix/redis"
)

var _ = Describe("Workflow", func() {
	Describe("Acknowledging items", func() {
		exchangeName, queueName := "test-workflow-exchange", "test-workflow-queue"
		prefix := "govuk_mirror_crawler_workflow_test"

		var (
			queueManager    *QueueManager
			queueManagerErr error
			ttlHashSet      *TTLHashSet
			ttlHashSetErr   error
		)

		BeforeEach(func() {
			ttlHashSet, ttlHashSetErr = NewTTLHashSet(prefix, "127.0.0.1:6379")
			Expect(ttlHashSetErr).To(BeNil())

			queueManager, queueManagerErr = NewQueueManager(
				"amqp://guest:guest@localhost:5672/",
				exchangeName,
				queueName)

			Expect(queueManagerErr).To(BeNil())
			Expect(queueManager).ToNot(BeNil())
		})

		AfterEach(func() {
			Expect(ttlHashSet.Close()).To(BeNil())
			Expect(purgeAllKeys(prefix, "127.0.0.1:6379")).To(BeNil())

			deleted, err := queueManager.Consumer.Channel.QueueDelete(queueName, false, false, true)
			Expect(err).To(BeNil())
			Expect(deleted).To(Equal(0))

			err = queueManager.Consumer.Channel.ExchangeDelete(exchangeName, false, true)
			Expect(err).To(BeNil())

			queueManager.Close()
		})

		Describe("AcknowledgeItem", func() {
			It("should read from a channel and add URLs to the hash set", func() {
				url := "https://www.gov.uk/foo"

				exists, err := ttlHashSet.Exists(url)
				Expect(err).To(BeNil())
				Expect(exists).To(BeFalse())

				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())

				outbound := make(chan *CrawlerMessageItem, 1)

				err = queueManager.Publish("#", "text/plain", url)
				Expect(err).To(BeNil())

				for item := range deliveries {
					outbound <- NewCrawlerMessageItem(item, "www.gov.uk", []string{})
					break
				}

				Expect(len(outbound)).To(Equal(1))

				go AcknowledgeItem(outbound, ttlHashSet)
				time.Sleep(time.Millisecond)

				Expect(len(outbound)).To(Equal(0))

				exists, err = ttlHashSet.Exists(url)
				Expect(err).To(BeNil())
				Expect(exists).To(BeTrue())

				// Close the channel to stop the goroutine for AcknowledgeItem.
				close(outbound)
			})
		})

		Describe("PublishURLs", func() {
			It("doesn't publish URLs that have already been crawled", func() {
				url := "https://www.gov.uk/government/organisations"

				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())
				Expect(len(deliveries)).To(Equal(0))

				_, err = ttlHashSet.Add(url)
				Expect(err).To(BeNil())

				publish := make(chan string, 1)
				outbound := make(chan []byte, 1)

				go func() {
					for item := range deliveries {
						outbound <- item.Body
					}
				}()
				go PublishURLs(ttlHashSet, queueManager, publish)
				time.Sleep(time.Millisecond)

				publish <- url
				time.Sleep(time.Millisecond)

				Expect(len(publish)).To(Equal(0))
				Expect(len(outbound)).To(Equal(0))

				// Close the channel to stop the goroutine for PublishURLs.
				close(publish)
				close(outbound)
			})

			It("publishes URLs that haven't been crawled yet", func() {
				url := "https://www.gov.uk/government/foo"

				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())
				Expect(len(deliveries)).To(Equal(0))

				publish := make(chan string, 1)
				outbound := make(chan []byte, 1)

				go func() {
					for item := range deliveries {
						outbound <- item.Body
					}
				}()
				go PublishURLs(ttlHashSet, queueManager, publish)
				time.Sleep(time.Millisecond)

				publish <- url
				time.Sleep(5 * time.Millisecond)

				Expect(len(publish)).To(Equal(0))
				Expect(len(outbound)).To(Equal(1))
				Expect(<-outbound).To(Equal([]byte(url)))

				close(publish)
				close(outbound)
			})
		})

		Describe("ReadFromQueue", func() {
			It("provides a way of converting AMQP bodies to CrawlerMessageItems", func() {
				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())

				outbound := ReadFromQueue(deliveries, ttlHashSet)
				Expect(len(outbound)).To(Equal(0))

				url := "https://www.foo.com/bar"
				err = queueManager.Publish("#", "text/plain", url)
				Expect(err).To(BeNil())

				item := <-outbound
				Expect(string(item.Body)).To(Equal(url))

				close(outbound)
			})
		})
	})
})

func purgeAllKeys(prefix string, address string) error {
	client, err := redis.Dial("tcp", address)
	if err != nil {
		return err
	}

	keys, err := client.Cmd("KEYS", prefix+"*").List()
	if err != nil || len(keys) <= 0 {
		return err
	}

	reply := client.Cmd("DEL", keys)
	if reply.Err != nil {
		return reply.Err
	}

	return nil
}