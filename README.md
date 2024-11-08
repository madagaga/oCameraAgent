# RTSP to WebRTC proxy

This project allows to forward RTSP streams to WebRTC peers.

## Configuration

The configuration is stored in a YAML file named `config.yaml`. The following keys are available:

* `Device.URL`: The URL of the RTSP stream to forward
* `MQTT.Username`: The username to use to connect to the MQTT broker
* `MQTT.Password`: The password to use to connect to the MQTT broker
* `MQTT.URL`: The URL of the MQTT broker
* `MQTT.Name`: The name to use to identify the proxy in the MQTT broker

## How to use

1. Build the project using `go build`
2. Copy the `config.yaml` to the same directory as the executable
3. Run the executable

The proxy will then forward the RTSP stream to WebRTC peers connected to the MQTT broker.

## MQTT signaling

The proxy and client use mqtt for signaling and google for turn/stun. 

### Declaring device 
The proxy sends a message to the MQTT broker with the topic `devices` and the payload
being a JSON object containing the following keys:

* `id`: The id of the device
* `name`: The name of the device
* `online`: true if the device is online, false otherwise
* `streaming`: true if the device is currently streaming, false otherwise
* `type`: The type of the device, always `sender` for the proxy.

This message is sent every time the proxy starts and every time a query request is received.

### Topics : 
The proxy and client use the following topics in the MQTT broker:

### `/devices` topic
The proxy sends a message to this topic with the payload being a JSON object containing the following keys:

* `id`: The id of the device
* `name`: The name of the device
* `online`: true if the device is online, false otherwise
* `streaming`: true if the device is currently streaming, false otherwise
* `type`: The type of the device, always `sender` for the proxy.

This message is sent every time the proxy starts and every time a query request is received.

### `/<clientID>` topic 
This topic is used to send / receive message between 2 peers
Every client is listening to the /clientId topic. 

#### `SDP` message
The proxy and client use this topic to send and receive SDP messages.

The message payload is a JSON object containing the following keys:

* `source`: The id of the device that sent the message
* `type`: The type of the message, always `sdp`
* `data`: The SDP message wich can be offer or answer. 

#### `ICE` message
The proxy and client use this topic to send and receive ICE messages.

The message payload is a JSON object containing the following keys:

* `source`: The id of the device that sent the message
* `type`: The type of the message, always `ice`
* `data`: The ICE message

### `Hangup` message
Send and receive hangup messages.

The message payload is a JSON object containing the following keys:

* `source`: The id of the device that sent the message
* `type`: The type of the message, always `hangup`

### `Query`message
Send and receive device query message 

* `source`: The id of the device that sent the message
* `type`: The type of the message, always `Query`