package rmq

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// DeadLetterSuffix names the parked-message queue derived from a work queue.
const DeadLetterSuffix = ".dlq"

// DeadLetterExchange returns the exchange a work queue's rejects route to.
func DeadLetterExchange(queue string) string { return queue + ".dlx" }

// DeadLetterQueue returns the queue that holds a work queue's rejects.
func DeadLetterQueue(queue string) string { return queue + DeadLetterSuffix }

// DeclareTopology declares the work queue together with its dead-letter path.
//
// A message that cannot be processed has to go somewhere. Requeueing it
// forever starves every valid order behind it; dropping it loses an order the
// customer has already paid for and leaves nothing to investigate. Parking it
// on a dead-letter queue does neither: the work queue keeps flowing, and the
// failed message is still there to be inspected and replayed once the cause is
// understood. A non-empty DLQ is also the single best alert this system has.
//
// Both the producer and the consumer call this, with identical arguments, so
// whichever starts first creates the topology and the other simply finds it.
func DeclareTopology(ch *amqp.Channel, queue string) error {
	exchange := DeadLetterExchange(queue)

	if err := ch.ExchangeDeclare(exchange, "fanout", true, false, false, false, nil); err != nil {
		return fmt.Errorf("rmq: declaring dead-letter exchange %q: %w", exchange, err)
	}

	parked := DeadLetterQueue(queue)
	if _, err := ch.QueueDeclare(parked, true, false, false, false, nil); err != nil {
		return fmt.Errorf("rmq: declaring dead-letter queue %q: %w", parked, err)
	}
	if err := ch.QueueBind(parked, "", exchange, false, nil); err != nil {
		return fmt.Errorf("rmq: binding %q to %q: %w", parked, exchange, err)
	}

	// Durable, so the broker restarting does not discard queued orders. The
	// messages themselves are published persistent, which is the other half of
	// that guarantee.
	_, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange": exchange,
	})
	if err != nil {
		return fmt.Errorf("rmq: declaring queue %q: %w", queue, err)
	}
	return nil
}

// DeclareTopic declares the durable topic exchange domain events are published
// to.
//
// Events and commands ride different topologies on purpose. A command — "ship
// this order" — has exactly one correct handler, and a work queue delivers it
// to exactly one consumer. An event — "this order was placed" — is a statement
// of fact that any number of services may care about, and a topic exchange
// lets each of them bind its own queue and get its own copy. Putting events on
// a work queue would mean the first consumer to grab a message denies it to
// everyone else, and adding a subscriber would silently break the existing
// ones.
func DeclareTopic(ch *amqp.Channel, exchange string) error {
	if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("rmq: declaring topic exchange %q: %w", exchange, err)
	}
	return nil
}

// DeclareSubscription declares a subscriber's own durable queue and binds it to
// the topic exchange for the given routing patterns.
//
// Each subscriber owns its queue, so a slow or stopped consumer accumulates a
// backlog of its own rather than affecting anyone else's delivery. The queue is
// durable and declared by the subscriber, which means events published while it
// was down are waiting when it returns.
func DeclareSubscription(ch *amqp.Channel, exchange, queue string, patterns []string) error {
	if err := DeclareTopic(ch, exchange); err != nil {
		return err
	}

	dlx := DeadLetterExchange(queue)
	if err := ch.ExchangeDeclare(dlx, "fanout", true, false, false, false, nil); err != nil {
		return fmt.Errorf("rmq: declaring dead-letter exchange %q: %w", dlx, err)
	}
	parked := DeadLetterQueue(queue)
	if _, err := ch.QueueDeclare(parked, true, false, false, false, nil); err != nil {
		return fmt.Errorf("rmq: declaring dead-letter queue %q: %w", parked, err)
	}
	if err := ch.QueueBind(parked, "", dlx, false, nil); err != nil {
		return fmt.Errorf("rmq: binding %q to %q: %w", parked, dlx, err)
	}

	if _, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange": dlx,
	}); err != nil {
		return fmt.Errorf("rmq: declaring subscription queue %q: %w", queue, err)
	}

	for _, pattern := range patterns {
		if err := ch.QueueBind(queue, pattern, exchange, false, nil); err != nil {
			return fmt.Errorf("rmq: binding %q to %q on %q: %w", queue, exchange, pattern, err)
		}
	}
	return nil
}
