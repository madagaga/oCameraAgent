# oCamera agent (RTSP to WebRTC proxy)

Agent qui relaie un flux RTSP vers un viewer WebRTC. La signalisation passe par MQTT selon le
protocole version 1, décrit dans `MQTT_PROTOCOL.md` (à la racine du projet, hors de ce dépôt).

## Configuration

Fichier YAML (`config.yaml`, voir `config.yaml.template`) :

| Clé | Description |
|---|---|
| `mqtt.url` | URL du broker, ex. `ssl://broker:8883` (TLS, certificat validé) |
| `mqtt.username` | **Identifiant de la caméra** (3-32 car. `[A-Za-z0-9_-]`), aussi `clientId` MQTT |
| `mqtt.password` | Mot de passe du compte caméra |
| `mqtt.ca` | (optionnel) Fichier PEM d'un CA personnalisé pour valider le broker |
| `device.name` | Nom affiché (défaut : l'identifiant) |
| `device.url` | URL RTSP source |
| `device.video` / `device.audio` | Codecs, ex. `H264` / `PCMU` |
| `ice.stun` | (optionnel) Liste de serveurs STUN ; défaut : STUN publics Google |

Les mots de passe et les identifiants RTSP ne sont jamais journalisés.

## Utilisation

1. `go build` (ou `build-musl.ps1` pour les cibles embarquées)
2. Placer `config.yaml` à côté de l'exécutable
3. Lancer `oCameraAgent [-config config.yaml] [-debug]`

## Protocole (résumé)

* `ocam/dev/{id}/state` : état retained (`online`, `busy`, `viewer`, `caps`...) + Last Will
* `ocam/dev/{id}/in/{viewer}` : messages reçus (`offer`, `ice`, `bye`, `cmd`)
* `ocam/cli/{viewer}/in/{id}` : messages émis (`answer`, `ice`, `bye`, `error`, `res`)
* Une seule session à la fois ; une seconde offre reçoit `error busy` sans perturber la session active.
* Au démarrage, l'agent tente de s'abonner à `ocam/dev/+/state` (interdit à une caméra) et avertit
  si le broker l'accepte (ACL non appliquée).

## Architecture

* `protocol.go` : enveloppe, types de messages, codes d'erreur
* `mqtt.go` : connexion, topics, Last Will, publication
* `agent.go` : machine de session (`idle` / `busy`), regroupement des candidats ICE
* `webrtc.go` : PeerConnection et relais RTP d'une session
* `rtsp_client.go` : client RTSP (TCP interleaved)
