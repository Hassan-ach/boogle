package messaging

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
	amqp "github.com/rabbitmq/amqp091-go"
)

type RabbitMQ struct {
	config *config.RabbitMqConfig
	logger *utils.Logger

	mu   sync.Mutex
	conn *amqp.Connection
}

type RabbitMQChannel struct {
	channel *amqp.Channel

	confirmOnce sync.Once
	confirmErr  error
	returns     <-chan amqp.Return
}

func NewRabbitMQ(conf *config.RabbitMqConfig, log *utils.Logger) (*RabbitMQ, error) {
	mq := &RabbitMQ{
		config: conf,
		logger: log,
	}

	if _, err := mq.ensureConn(); err != nil {
		return nil, err
	}
	if err := mq.DeclareQueue(conf.JobQueue); err != nil {
		return nil, err
	}
	return mq, nil
}

// ensureConn returns a live connection, dialing a replacement when the previous
// one has died. A broker restart used to make every later crawl fail forever
// because the connection was dialed exactly once at startup.
func (m *RabbitMQ) ensureConn() (*amqp.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil && !m.conn.IsClosed() {
		return m.conn, nil
	}

	conn, err := amqp.Dial(getRabbitURL(m.config))
	if err != nil {
		return nil, err
	}
	m.conn = conn
	return conn, nil
}

func (m *RabbitMQ) NewMessagingChannel() (MessagingChannel, error) {
	conn, err := m.ensureConn()
	if err != nil {
		return nil, err
	}

	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	return &RabbitMQChannel{
		channel: ch,
	}, nil
}

func (m *RabbitMQ) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
}

// Publish sends one indexer job as a persistent message and waits for the
// broker's publisher confirm.
//
// A plain Publish returning nil only means the bytes reached the socket. The
// spider retires the URL after this call, so a job lost to a broker restart was
// never retried and the page silently stayed out of the index (H-09). Persistent
// delivery survives a broker restart; the confirm is what makes success real.
func (m *RabbitMQChannel) Publish(ctx context.Context, queueName string, body []byte) error {
	if err := m.enableConfirms(); err != nil {
		return err
	}

	// mandatory=true makes an unroutable message come back as a basic.return
	// instead of a plain ack. Without it, a renamed or deleted queue produced a
	// confirmed publish and a job that existed nowhere.
	confirmation, err := m.channel.PublishWithDeferredConfirm(
		"",
		queueName,
		true,
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
		},
	)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("waiting for the publish confirm: %w", err)
	}

	// The broker's return frame is dispatched before the ack on the same channel,
	// so a returned message is already buffered here.
	select {
	case returned := <-m.returns:
		return fmt.Errorf("the broker returned the indexer job: %d %s",
			returned.ReplyCode, returned.ReplyText)
	default:
	}

	if !acked {
		return errors.New("the broker nacked the indexer job")
	}
	return nil
}

func (m *RabbitMQChannel) enableConfirms() error {
	m.confirmOnce.Do(func() {
		if err := m.channel.Confirm(false); err != nil {
			m.confirmErr = err
			return
		}
		m.returns = m.channel.NotifyReturn(make(chan amqp.Return, 1))
	})
	return m.confirmErr
}

func (m *RabbitMQChannel) Consume(queueName string, handler func([]byte) error) error {
	msgs, err := m.channel.Consume(
		queueName,
		"",
		false,
		false,
		false,
		false,
		nil,
	)

	if err != nil {
		return err
	}

	for msg := range msgs {
		handlerErr := handler(msg.Body)
		if handlerErr != nil {
			msg.Nack(false, true)
			continue
		}
		msg.Ack(false)
	}
	return nil
}

func (m *RabbitMQ) DeclareQueue(queueName string) error {
	ch, err := m.conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	_, err = ch.QueueDeclare(
		queueName,
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}
	return nil
}

func (m *RabbitMQChannel) Close() {
	m.channel.Close()
}

// getRabbitURL is the one place the password is handed to a client. It is a
// URL, so the secret is unavoidably in the string: do not log the result.
func getRabbitURL(conf *config.RabbitMqConfig) string {
	port := strconv.Itoa(conf.Port)
	return "amqp://" + conf.User + ":" + conf.Password.Reveal() + "@" + conf.Host + ":" + port
}
