// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"golang.org/x/exp/slices"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	layerSelectionTolerance = 0.9
)

var (
	ErrNotOpen    = errors.New("track is not open")
	ErrNoReceiver = errors.New("cannot subscribe without a receiver in place")
)

// ------------------------------------------------------

type mediaTrackReceiverState int

const (
	mediaTrackReceiverStateOpen mediaTrackReceiverState = iota
	mediaTrackReceiverStateClosing
	mediaTrackReceiverStateClosed
)

func (m mediaTrackReceiverState) String() string {
	switch m {
	case mediaTrackReceiverStateOpen:
		return "OPEN"
	case mediaTrackReceiverStateClosing:
		return "CLOSING"
	case mediaTrackReceiverStateClosed:
		return "CLOSED"
	default:
		return fmt.Sprintf("%d", int(m))
	}
}

// -----------------------------------------------------

type simulcastReceiver struct {
	sfu.TrackReceiver
	priority int
}

func (r *simulcastReceiver) Priority() int {
	return r.priority
}

type MediaTrackReceiverParams struct {
	MediaTrack          types.MediaTrack
	IsRelayed           bool
	ParticipantID       livekit.ParticipantID
	ParticipantIdentity livekit.ParticipantIdentity
	ParticipantVersion  uint32
	ReceiverConfig      ReceiverConfig
	SubscriberConfig    DirectionConfig
	AudioConfig         config.AudioConfig
	Telemetry           telemetry.TelemetryService
	Logger              logger.Logger
}

type MediaTrackReceiver struct {
	params MediaTrackReceiverParams

	lock            sync.RWMutex
	receivers       []*simulcastReceiver
	trackInfo       *livekit.TrackInfo
	potentialCodecs []webrtc.RTPCodecParameters
	state           mediaTrackReceiverState

	onSetupReceiver     func(mime string)
	onMediaLossFeedback func(dt *sfu.DownTrack, report *rtcp.ReceiverReport)
	onClose             []func()

	*MediaTrackSubscriptions
}

func NewMediaTrackReceiver(params MediaTrackReceiverParams, ti *livekit.TrackInfo) *MediaTrackReceiver {
	t := &MediaTrackReceiver{
		params:    params,
		trackInfo: proto.Clone(ti).(*livekit.TrackInfo),
		state:     mediaTrackReceiverStateOpen,
	}

	t.MediaTrackSubscriptions = NewMediaTrackSubscriptions(MediaTrackSubscriptionsParams{
		MediaTrack:       params.MediaTrack,
		IsRelayed:        params.IsRelayed,
		ReceiverConfig:   params.ReceiverConfig,
		SubscriberConfig: params.SubscriberConfig,
		Telemetry:        params.Telemetry,
		Logger:           params.Logger,
	})
	t.MediaTrackSubscriptions.OnDownTrackCreated(t.onDownTrackCreated)

	if t.trackInfo.Muted {
		t.SetMuted(true)
	}
	return t
}

func (t *MediaTrackReceiver) Restart() {
	t.lock.RLock()
	hq := buffer.VideoQualityToSpatialLayer(livekit.VideoQuality_HIGH, t.trackInfo)
	t.lock.RUnlock()

	for _, receiver := range t.loadReceivers() {
		receiver.SetMaxExpectedSpatialLayer(hq)
	}
}

func (t *MediaTrackReceiver) OnSetupReceiver(f func(mime string)) {
	t.lock.Lock()
	t.onSetupReceiver = f
	t.lock.Unlock()
}

func (t *MediaTrackReceiver) SetupReceiver(receiver sfu.TrackReceiver, priority int, mid string) {
	t.lock.Lock()
	if t.state != mediaTrackReceiverStateOpen {
		t.params.Logger.Warnw("cannot set up receiver on a track not open", nil)
		t.lock.Unlock()
		return
	}

	receivers := slices.Clone(t.receivers)

	// codec position maybe taken by DummyReceiver, check and upgrade to WebRTCReceiver
	var upgradeReceiver bool
	for _, r := range receivers {
		if strings.EqualFold(r.Codec().MimeType, receiver.Codec().MimeType) {
			if d, ok := r.TrackReceiver.(*DummyReceiver); ok {
				d.Upgrade(receiver)
				upgradeReceiver = true
				break
			}
		}
	}
	if !upgradeReceiver {
		receivers = append(receivers, &simulcastReceiver{TrackReceiver: receiver, priority: priority})
	}

	sort.Slice(receivers, func(i, j int) bool {
		return receivers[i].Priority() < receivers[j].Priority()
	})

	if mid != "" {
		if priority == 0 {
			t.trackInfo.MimeType = receiver.Codec().MimeType
			t.trackInfo.Mid = mid
		}

		for i, ci := range t.trackInfo.Codecs {
			if i == priority {
				ci.MimeType = receiver.Codec().MimeType
				ci.Mid = mid
				break
			}
		}
	}

	t.receivers = receivers
	onSetupReceiver := t.onSetupReceiver
	t.lock.Unlock()

	var receiverCodecs []string
	for _, r := range receivers {
		receiverCodecs = append(receiverCodecs, r.Codec().MimeType)
	}
	t.params.Logger.Debugw(
		"setup receiver",
		"mime", receiver.Codec().MimeType,
		"priority", priority,
		"receivers", receiverCodecs,
		"mid", mid,
	)

	if onSetupReceiver != nil {
		onSetupReceiver(receiver.Codec().MimeType)
	}
}

func (t *MediaTrackReceiver) SetPotentialCodecs(codecs []webrtc.RTPCodecParameters, headers []webrtc.RTPHeaderExtensionParameter) {
	// The potential codecs have not published yet, so we can't get the actual Extensions, the client/browser uses same extensions
	// for all video codecs so we assume they will have same extensions as the primary codec except for the dependency descriptor
	// that is munged in svc codec.
	headersWithoutDD := make([]webrtc.RTPHeaderExtensionParameter, 0, len(headers))
	for _, h := range headers {
		if h.URI != dependencydescriptor.ExtensionURI {
			headersWithoutDD = append(headersWithoutDD, h)
		}
	}
	t.lock.Lock()
	receivers := slices.Clone(t.receivers)
	t.potentialCodecs = codecs
	for i, c := range codecs {
		var exist bool
		for _, r := range receivers {
			if strings.EqualFold(c.MimeType, r.Codec().MimeType) {
				exist = true
				break
			}
		}
		if !exist {
			extHeaders := headers
			if !sfu.IsSvcCodec(c.MimeType) {
				extHeaders = headersWithoutDD
			}
			receivers = append(receivers, &simulcastReceiver{
				TrackReceiver: NewDummyReceiver(livekit.TrackID(t.trackInfo.Sid), string(t.PublisherID()), c, extHeaders),
				priority:      i,
			})
		}
	}
	sort.Slice(receivers, func(i, j int) bool {
		return receivers[i].Priority() < receivers[j].Priority()
	})
	t.receivers = receivers
	t.lock.Unlock()
}

func (t *MediaTrackReceiver) ClearReceiver(mime string, willBeResumed bool) {
	t.lock.Lock()
	receivers := slices.Clone(t.receivers)
	for idx, receiver := range receivers {
		if strings.EqualFold(receiver.Codec().MimeType, mime) {
			receivers[idx] = receivers[len(receivers)-1]
			receivers[len(receivers)-1] = nil
			receivers = receivers[:len(receivers)-1]
			break
		}
	}
	t.receivers = receivers
	t.lock.Unlock()

	t.removeAllSubscribersForMime(mime, willBeResumed)
}

func (t *MediaTrackReceiver) ClearAllReceivers(willBeResumed bool) {
	t.params.Logger.Debugw("clearing all receivers")
	t.lock.Lock()
	receivers := t.receivers
	t.receivers = nil
	t.lock.Unlock()

	for _, r := range receivers {
		t.removeAllSubscribersForMime(r.Codec().MimeType, willBeResumed)
	}
}

func (t *MediaTrackReceiver) OnMediaLossFeedback(f func(dt *sfu.DownTrack, rr *rtcp.ReceiverReport)) {
	t.onMediaLossFeedback = f
}

func (t *MediaTrackReceiver) IsOpen() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	if t.state != mediaTrackReceiverStateOpen {
		return false
	}
	// If any one of the receivers has entered closed state, we would not consider the track open
	for _, receiver := range t.receivers {
		if receiver.IsClosed() {
			return false
		}
	}
	return true
}

func (t *MediaTrackReceiver) SetClosing() {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.state == mediaTrackReceiverStateOpen {
		t.state = mediaTrackReceiverStateClosing
	}
}

func (t *MediaTrackReceiver) TryClose() bool {
	t.lock.RLock()
	if t.state == mediaTrackReceiverStateClosed {
		t.lock.RUnlock()
		return true
	}

	for _, receiver := range t.receivers {
		if dr, _ := receiver.TrackReceiver.(*DummyReceiver); dr != nil && dr.Receiver() != nil {
			t.lock.RUnlock()
			return false
		}
	}
	t.lock.RUnlock()
	t.Close()

	return true
}

func (t *MediaTrackReceiver) Close() {
	t.lock.Lock()
	if t.state == mediaTrackReceiverStateClosed {
		t.lock.Unlock()
		return
	}

	t.state = mediaTrackReceiverStateClosed
	onclose := t.onClose
	t.lock.Unlock()

	for _, f := range onclose {
		f()
	}
}

func (t *MediaTrackReceiver) ID() livekit.TrackID {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return livekit.TrackID(t.trackInfo.Sid)
}

func (t *MediaTrackReceiver) Kind() livekit.TrackType {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Type
}

func (t *MediaTrackReceiver) Source() livekit.TrackSource {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Source
}

func (t *MediaTrackReceiver) Stream() string {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Stream
}

func (t *MediaTrackReceiver) PublisherID() livekit.ParticipantID {
	return t.params.ParticipantID
}

func (t *MediaTrackReceiver) PublisherIdentity() livekit.ParticipantIdentity {
	return t.params.ParticipantIdentity
}

func (t *MediaTrackReceiver) PublisherVersion() uint32 {
	return t.params.ParticipantVersion
}

func (t *MediaTrackReceiver) IsSimulcast() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Simulcast
}

func (t *MediaTrackReceiver) SetSimulcast(simulcast bool) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.trackInfo.Simulcast = simulcast
}

func (t *MediaTrackReceiver) Name() string {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Name
}

func (t *MediaTrackReceiver) IsMuted() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Muted
}

func (t *MediaTrackReceiver) SetMuted(muted bool) {
	t.lock.Lock()
	t.trackInfo.Muted = muted
	t.lock.Unlock()

	for _, receiver := range t.loadReceivers() {
		receiver.SetUpTrackPaused(muted)
	}

	t.MediaTrackSubscriptions.SetMuted(muted)
}

func (t *MediaTrackReceiver) AddOnClose(f func()) {
	if f == nil {
		return
	}

	t.lock.Lock()
	t.onClose = append(t.onClose, f)
	t.lock.Unlock()
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackReceiver) AddSubscriber(sub types.LocalParticipant) (types.SubscribedTrack, error) {
	t.lock.RLock()
	if t.state != mediaTrackReceiverStateOpen {
		t.lock.RUnlock()
		return nil, ErrNotOpen
	}

	receivers := t.receivers
	potentialCodecs := make([]webrtc.RTPCodecParameters, len(t.potentialCodecs))
	copy(potentialCodecs, t.potentialCodecs)
	t.lock.RUnlock()

	if len(receivers) == 0 {
		// cannot add, no receiver
		return nil, ErrNoReceiver
	}

	for _, receiver := range receivers {
		codec := receiver.Codec()
		var found bool
		for _, pc := range potentialCodecs {
			if codec.MimeType == pc.MimeType {
				found = true
				break
			}
		}
		if !found {
			potentialCodecs = append(potentialCodecs, codec)
		}
	}

	streamId := string(t.PublisherID())
	if sub.ProtocolVersion().SupportsPackedStreamId() {
		// when possible, pack both IDs in streamID to allow new streams to be generated
		// react-native-webrtc still uses stream based APIs and require this
		streamId = PackStreamID(t.PublisherID(), t.ID())
	}

	tLogger := LoggerWithTrack(sub.GetLogger(), t.ID(), t.params.IsRelayed)
	wr := NewWrappedReceiver(WrappedReceiverParams{
		Receivers:      receivers,
		TrackID:        t.ID(),
		StreamId:       streamId,
		UpstreamCodecs: potentialCodecs,
		Logger:         tLogger,
		DisableRed:     t.trackInfo.GetDisableRed() || !t.params.AudioConfig.ActiveREDEncoding,
	})
	return t.MediaTrackSubscriptions.AddSubscriber(sub, wr)
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackReceiver) RemoveSubscriber(subscriberID livekit.ParticipantID, willBeResumed bool) {
	_ = t.MediaTrackSubscriptions.RemoveSubscriber(subscriberID, willBeResumed)
}

func (t *MediaTrackReceiver) removeAllSubscribersForMime(mime string, willBeResumed bool) {
	t.params.Logger.Debugw("removing all subscribers for mime", "mime", mime)
	for _, subscriberID := range t.MediaTrackSubscriptions.GetAllSubscribersForMime(mime) {
		t.RemoveSubscriber(subscriberID, willBeResumed)
	}
}

func (t *MediaTrackReceiver) RevokeDisallowedSubscribers(allowedSubscriberIdentities []livekit.ParticipantIdentity) []livekit.ParticipantIdentity {
	var revokedSubscriberIdentities []livekit.ParticipantIdentity

	// LK-TODO: large number of subscribers needs to be solved for this loop
	for _, subTrack := range t.MediaTrackSubscriptions.getAllSubscribedTracks() {
		found := false
		for _, allowedIdentity := range allowedSubscriberIdentities {
			if subTrack.SubscriberIdentity() == allowedIdentity {
				found = true
				break
			}
		}

		if !found {
			t.params.Logger.Infow("revoking subscription",
				"subscriber", subTrack.SubscriberIdentity(),
				"subscriberID", subTrack.SubscriberID(),
			)
			t.RemoveSubscriber(subTrack.SubscriberID(), false)
			revokedSubscriberIdentities = append(revokedSubscriberIdentities, subTrack.SubscriberIdentity())
		}
	}

	return revokedSubscriberIdentities
}

func (t *MediaTrackReceiver) updateTrackInfoOfReceivers() {
	t.lock.RLock()
	ti := proto.Clone(t.trackInfo).(*livekit.TrackInfo)
	t.lock.RUnlock()

	for _, r := range t.loadReceivers() {
		r.UpdateTrackInfo(ti)
	}
}

func (t *MediaTrackReceiver) SetLayerSsrc(mime string, rid string, ssrc uint32) {
	t.lock.Lock()
	layer := buffer.RidToSpatialLayer(rid, t.trackInfo)
	if layer == buffer.InvalidLayerSpatial {
		// non-simulcast case will not have `rid`
		layer = 0
	}
	quality := buffer.SpatialLayerToVideoQuality(layer, t.trackInfo)
	// set video layer ssrc info
	for i, ci := range t.trackInfo.Codecs {
		if !strings.EqualFold(ci.MimeType, mime) {
			continue
		}

		// if origin layer has ssrc, don't override it
		var matchingLayer *livekit.VideoLayer
		ssrcFound := false
		for _, l := range ci.Layers {
			if l.Quality == quality {
				matchingLayer = l
				if l.Ssrc != 0 {
					ssrcFound = true
				}
				break
			}
		}
		if !ssrcFound && matchingLayer != nil {
			matchingLayer.Ssrc = ssrc
		}

		// for client don't use simulcast codecs (old client version or single codec)
		if i == 0 {
			t.trackInfo.Layers = ci.Layers
		}
		break
	}
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateCodecCid(codecs []*livekit.SimulcastCodec) {
	t.lock.Lock()
	for _, c := range codecs {
		for _, origin := range t.trackInfo.Codecs {
			if strings.Contains(origin.MimeType, c.Codec) {
				origin.Cid = c.Cid
				break
			}
		}
	}
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	updateMute := false
	clonedInfo := proto.Clone(ti).(*livekit.TrackInfo)

	t.lock.Lock()
	// patch Mid and SSRC of codecs/layers by keeping original if available
	for i, ci := range clonedInfo.Codecs {
		for _, originCi := range t.trackInfo.Codecs {
			if !strings.EqualFold(ci.MimeType, originCi.MimeType) {
				continue
			}

			if originCi.Mid != "" {
				ci.Mid = originCi.Mid
			}

			for _, layer := range ci.Layers {
				for _, originLayer := range originCi.Layers {
					if layer.Quality == originLayer.Quality {
						if originLayer.Ssrc != 0 {
							layer.Ssrc = originLayer.Ssrc
						}
						break
					}
				}
			}
			break
		}

		// for client don't use simulcast codecs (old client version or single codec)
		if i == 0 {
			clonedInfo.Layers = ci.Layers
		}
	}
	if t.trackInfo.Muted != clonedInfo.Muted {
		updateMute = true
	}
	t.trackInfo = clonedInfo
	t.lock.Unlock()

	if updateMute {
		t.SetMuted(clonedInfo.Muted)
	}

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateVideoLayers(layers []*livekit.VideoLayer) {
	t.lock.Lock()
	// set video layer ssrc info
	for i, ci := range t.trackInfo.Codecs {
		originLayers := ci.Layers
		ci.Layers = []*livekit.VideoLayer{}
		for layerIdx, layer := range layers {
			ci.Layers = append(ci.Layers, proto.Clone(layer).(*livekit.VideoLayer))
			for _, l := range originLayers {
				if l.Quality == ci.Layers[layerIdx].Quality {
					if l.Ssrc != 0 {
						ci.Layers[layerIdx].Ssrc = l.Ssrc
					}
					break
				}
			}
		}

		// for client don't use simulcast codecs (old client version or single codec)
		if i == 0 {
			t.trackInfo.Layers = ci.Layers
		}
	}
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()
	t.MediaTrackSubscriptions.UpdateVideoLayers()
}

func (t *MediaTrackReceiver) TrackInfo() *livekit.TrackInfo {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo
}

func (t *MediaTrackReceiver) TrackInfoClone() *livekit.TrackInfo {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return proto.Clone(t.trackInfo).(*livekit.TrackInfo)
}

func (t *MediaTrackReceiver) NotifyMaxLayerChange(maxLayer int32) {
	t.lock.RLock()
	quality := buffer.SpatialLayerToVideoQuality(maxLayer, t.trackInfo)
	ti := &livekit.TrackInfo{
		Sid:    t.trackInfo.Sid,
		Type:   t.trackInfo.Type,
		Layers: []*livekit.VideoLayer{{Quality: quality}},
	}
	if quality != livekit.VideoQuality_OFF {
		for _, layer := range t.trackInfo.Layers {
			if layer.Quality == quality {
				ti.Layers[0].Width = layer.Width
				ti.Layers[0].Height = layer.Height
				break
			}
		}
	}
	t.lock.RUnlock()

	t.params.Telemetry.TrackPublishedUpdate(context.Background(), t.PublisherID(), ti)
}

// GetQualityForDimension finds the closest quality to use for desired dimensions
// affords a 20% tolerance on dimension
func (t *MediaTrackReceiver) GetQualityForDimension(width, height uint32) livekit.VideoQuality {
	quality := livekit.VideoQuality_HIGH
	if t.Kind() == livekit.TrackType_AUDIO {
		return quality
	}

	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.trackInfo.Height == 0 {
		return quality
	}
	origSize := t.trackInfo.Height
	requestedSize := height
	if t.trackInfo.Width < t.trackInfo.Height {
		// for portrait videos
		origSize = t.trackInfo.Width
		requestedSize = width
	}

	// default sizes representing qualities low - high
	layerSizes := []uint32{180, 360, origSize}
	var providedSizes []uint32
	for _, layer := range t.trackInfo.Layers {
		providedSizes = append(providedSizes, layer.Height)
	}
	if len(providedSizes) > 0 {
		layerSizes = providedSizes
		// comparing height always
		requestedSize = height
		sort.Slice(layerSizes, func(i, j int) bool {
			return layerSizes[i] < layerSizes[j]
		})
	}

	// finds the highest layer with smallest dimensions that still satisfy client demands
	requestedSize = uint32(float32(requestedSize) * layerSelectionTolerance)
	for i, s := range layerSizes {
		quality = livekit.VideoQuality(i)
		if i == len(layerSizes)-1 {
			break
		} else if s >= requestedSize && s != layerSizes[i+1] {
			break
		}
	}

	return quality
}

func (t *MediaTrackReceiver) GetAudioLevel() (float64, bool) {
	receiver := t.PrimaryReceiver()
	if receiver == nil {
		return 0, false
	}

	return receiver.GetAudioLevel()
}

func (t *MediaTrackReceiver) onDownTrackCreated(downTrack *sfu.DownTrack) {
	if t.Kind() == livekit.TrackType_AUDIO {
		downTrack.AddReceiverReportListener(func(dt *sfu.DownTrack, rr *rtcp.ReceiverReport) {
			if t.onMediaLossFeedback != nil {
				t.onMediaLossFeedback(dt, rr)
			}
		})
	}
}

func (t *MediaTrackReceiver) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"ID":       t.ID(),
		"Kind":     t.Kind().String(),
		"PubMuted": t.IsMuted(),
	}

	info["DownTracks"] = t.MediaTrackSubscriptions.DebugInfo()

	for _, receiver := range t.loadReceivers() {
		info[receiver.Codec().MimeType] = receiver.DebugInfo()
	}

	return info
}

func (t *MediaTrackReceiver) PrimaryReceiver() sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if len(t.receivers) == 0 {
		return nil
	}
	if dr, ok := t.receivers[0].TrackReceiver.(*DummyReceiver); ok {
		return dr.Receiver()
	}
	return t.receivers[0].TrackReceiver
}

func (t *MediaTrackReceiver) Receiver(mime string) sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	for _, r := range t.receivers {
		if strings.EqualFold(r.Codec().MimeType, mime) {
			if dr, ok := r.TrackReceiver.(*DummyReceiver); ok {
				return dr.Receiver()
			}
			return r.TrackReceiver
		}
	}
	return nil
}

func (t *MediaTrackReceiver) Receivers() []sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()
	receivers := make([]sfu.TrackReceiver, len(t.receivers))
	for i, r := range t.receivers {
		receivers[i] = r.TrackReceiver
	}
	return receivers
}

func (t *MediaTrackReceiver) loadReceivers() []*simulcastReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.receivers
}

func (t *MediaTrackReceiver) SetRTT(rtt uint32) {
	for _, r := range t.loadReceivers() {
		if wr, ok := r.TrackReceiver.(*sfu.WebRTCReceiver); ok {
			wr.SetRTT(rtt)
		}
	}
}

func (t *MediaTrackReceiver) GetTemporalLayerForSpatialFps(spatial int32, fps uint32, mime string) int32 {
	receiver := t.Receiver(mime)
	if receiver == nil {
		return buffer.DefaultMaxLayerTemporal
	}

	layerFps := receiver.GetTemporalLayerFpsForSpatial(spatial)
	requestFps := float32(fps) * layerSelectionTolerance
	for i, f := range layerFps {
		if requestFps <= f {
			return int32(i)
		}
	}
	return buffer.DefaultMaxLayerTemporal
}

func (t *MediaTrackReceiver) IsEncrypted() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Encryption != livekit.Encryption_NONE
}

func (t *MediaTrackReceiver) GetTrackStats() *livekit.RTPStats {
	receivers := t.loadReceivers()
	stats := make([]*livekit.RTPStats, 0, len(receivers))
	for _, receiver := range receivers {
		receiverStats := receiver.GetTrackStats()
		if receiverStats != nil {
			stats = append(stats, receiverStats)
		}
	}

	return buffer.AggregateRTPStats(stats)
}

// ---------------------------
