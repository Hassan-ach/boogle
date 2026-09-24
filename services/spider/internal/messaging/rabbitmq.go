package messaging

import (
	"strconv"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
	amqp "github.com/rabbitmq/amqp091-go"
)

type RabbitMQ struct {
	config *config.RabbitMqConfig
	conn   *amqp.Connection
	logger *utils.Logger
}

type RabbitMQChannel struct {
	channel *amqp.Channel
}

func NewRabbitMQ(conf *config.RabbitMqConfig, log *utils.Logger) (*RabbitMQ, error) {

	rabbitURL := getRabbitURL(conf)

	conn, err := amqp.Dial(rabbitURL)
	if err != nil {
		return nil, err
	}

	mq := &RabbitMQ{
		config: conf,
		conn:   conn,
		logger: log,
	}

	err = mq.DeclareQueue("indexer.jobs")
	if err != nil {
		return nil, err
	}
	return mq, nil

}

func (m *RabbitMQ) NewMessagingChannel() (MessagingChannel, error) {

	ch, err := m.conn.Channel()
	if err != nil {
		return nil, err
	}
	return &RabbitMQChannel{
		channel: ch,
	}, nil

}

func (m *RabbitMQ) Close() {
	m.conn.Close()
}

func (m *RabbitMQChannel) Publish(queueName string, body []byte) error {
	err := m.channel.Publish(
		"",
		queueName,
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		},
	)
	if err != nil {
		return err
	}

	return nil
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

func getRabbitURL(conf *config.RabbitMqConfig) string {
	port := strconv.Itoa(conf.Port)
	return "amqp://" + conf.User + ":" + conf.Password + "@" + conf.Host + ":" + port
}
