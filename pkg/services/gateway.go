package services

import (
	"context"
	"encoding/json"
	"log"
	"path"
	"sync"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/pojntfx/dudirekta/pkg/rpc"
	mqttapi "github.com/pojntfx/green-guardian-gateway/pkg/api/mqtt"
)

type GatewayRemote struct {
	RegisterFans                  func(ctx context.Context, roomIDs []string) error
	UnregisterFans                func(ctx context.Context, roomIDs []string) error
	ForwardTemperatureMeasurement func(ctx context.Context, roomID string, measurement, defaultValue int) error

	RegisterSprinklers         func(ctx context.Context, plantIDs []string) error
	UnregisterSprinklers       func(ctx context.Context, plantIDs []string) error
	ForwardMoistureMeasurement func(ctx context.Context, plantID string, measurement, defaultValue int) error
}

type Gateway struct {
	verbose bool

	errs chan error

	broker    mqtt.Client
	thingName string

	fans     map[string]string
	fansLock sync.Mutex

	sprinklers     map[string]string
	sprinklersLock sync.Mutex

	Peers func() map[string]HubRemote
}

func NewGateway(
	verbose bool,
	ctx context.Context,
	broker mqtt.Client,
	thingName string,
) *Gateway {
	return &Gateway{
		verbose: verbose,

		errs: make(chan error),

		fans: map[string]string{},

		sprinklers: map[string]string{},

		broker:    broker,
		thingName: thingName,
	}
}

func (w *Gateway) RegisterFans(ctx context.Context, roomIDs []string) error {
	if w.verbose {
		log.Printf("RegisterFans(roomIDs=%v)", roomIDs)
	}

	peerID := rpc.GetRemoteID(ctx)

	w.fansLock.Lock()
	defer w.fansLock.Unlock()

	for _, roomID := range roomIDs {
		w.fans[roomID] = peerID
	}

	return nil
}

func (w *Gateway) UnregisterFans(ctx context.Context, roomIDs []string) error {
	if w.verbose {
		log.Printf("UnregisterFans(roomIDs=%v)", roomIDs)
	}

	w.fansLock.Lock()
	defer w.fansLock.Unlock()

	for _, roomID := range roomIDs {
		delete(w.fans, roomID)
	}

	return nil
}

func (w *Gateway) RegisterSprinklers(ctx context.Context, plantIDs []string) error {
	if w.verbose {
		log.Printf("RegisterSprinklers(plantIDs=%v)", plantIDs)
	}

	peerID := rpc.GetRemoteID(ctx)

	w.sprinklersLock.Lock()
	defer w.sprinklersLock.Unlock()

	for _, plantID := range plantIDs {
		w.sprinklers[plantID] = peerID
	}

	return nil
}

func (w *Gateway) UnregisterSprinklers(ctx context.Context, plantIDs []string) error {
	if w.verbose {
		log.Printf("UnregisterSpriklers(plantIDs=%v)", plantIDs)
	}

	w.sprinklersLock.Lock()
	defer w.sprinklersLock.Unlock()

	for _, plantID := range plantIDs {
		delete(w.sprinklers, plantID)
	}

	return nil
}

func (w *Gateway) ForwardTemperatureMeasurement(ctx context.Context, roomID string, measurement, defaultValue int) error {
	if w.verbose {
		log.Printf("ForwardTemperatureMeasurement(roomIDs=%v, measurement=%v, defaultValue=%v)", roomID, measurement, defaultValue)
	}

	msg, err := json.Marshal(mqttapi.TemperatureMeasurement{
		Measurement:  measurement,
		DefaultValue: defaultValue,
	})
	if err != nil {
		return err
	}

	if token := w.broker.Publish(
		path.Join("/gateways", w.thingName, "rooms", roomID, "temperature"),
		0,
		false,
		msg,
	); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	return nil
}

func (w *Gateway) ForwardMoistureMeasurement(ctx context.Context, plantID string, measurement, defaultValue int) error {
	if w.verbose {
		log.Printf("ForwardMoistureMeasurement(plantIDs=%v, measurement=%v, defaultValue=%v)", plantID, measurement, defaultValue)
	}

	msg, err := json.Marshal(mqttapi.MoistureMeasurement{
		Measurement:  measurement,
		DefaultValue: defaultValue,
	})
	if err != nil {
		return err
	}

	if token := w.broker.Publish(
		path.Join("/gateways", w.thingName, "plants", plantID, "moisture"),
		0,
		false,
		msg,
	); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	return nil
}

func OpenGateway(gateway *Gateway, ctx context.Context) error {
	if token := gateway.broker.Subscribe(
		path.Join("/gateways", gateway.thingName, "rooms", "+", "fan"),
		0,
		func(client mqtt.Client, msg mqtt.Message) {
			gateway.fansLock.Lock()
			defer gateway.fansLock.Unlock()

			basePath, _ := path.Split(msg.Topic())

			roomID := path.Base(basePath)

			peerID, ok := gateway.fans[roomID]
			if !ok {
				gateway.errs <- ErrNoSuchRoom

				return
			}

			hub, ok := gateway.Peers()[peerID]
			if !ok {
				gateway.errs <- ErrNoSuchRoom

				return
			}

			fanState := &mqttapi.FanState{}
			if err := json.Unmarshal(msg.Payload(), &fanState); err != nil {
				gateway.errs <- err

				return
			}

			if err := hub.SetFanOn(ctx, roomID, fanState.On); err != nil {
				gateway.errs <- err

				return
			}
		},
	); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	if token := gateway.broker.Subscribe(
		path.Join("/gateways", gateway.thingName, "plants", "+", "sprinkler"),
		0,
		func(client mqtt.Client, msg mqtt.Message) {
			gateway.sprinklersLock.Lock()
			defer gateway.sprinklersLock.Unlock()

			basePath, _ := path.Split(msg.Topic())

			plantID := path.Base(basePath)

			peerID, ok := gateway.sprinklers[plantID]
			if !ok {
				gateway.errs <- ErrNoSuchPlant

				return
			}

			hub, ok := gateway.Peers()[peerID]
			if !ok {
				gateway.errs <- ErrNoSuchPlant

				return
			}

			sprinklerState := &mqttapi.SprinklerState{}
			if err := json.Unmarshal(msg.Payload(), &sprinklerState); err != nil {
				gateway.errs <- err

				return
			}

			if err := hub.SetSprinklerOn(ctx, plantID, sprinklerState.On); err != nil {
				gateway.errs <- err

				return
			}
		},
	); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	return nil
}

func WaitGateway(gateway *Gateway) error {
	for err := range gateway.errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func CloseGateway(gateway *Gateway) error {
	if token := gateway.broker.Unsubscribe(
		path.Join("/gateways", gateway.thingName, "rooms", "+", "fan"),
	); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	if token := gateway.broker.Unsubscribe(
		path.Join("/gateways", gateway.thingName, "rooms", "+", "sprinkler"),
	); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	close(gateway.errs)

	return nil
}
