package spot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Direct-to-Sonos playback path. Bypasses Spotify Connect entirely by
// driving Sonos's own native Spotify integration over UPnP/SOAP. Works on
// every Sonos speaker that has Spotify linked in the Sonos app, including
// older hardware (Play:1, Play:3, One SL, etc.) that doesn't register as
// a Spotify Connect endpoint.

const (
	ssdpAddr     = "239.255.255.250:1900"
	sonosSearch  = "urn:schemas-upnp-org:device:ZonePlayer:1"
	sonosPort    = 1400
	discoveryFor = 3 * time.Second
)

// Zone describes a single Sonos zone (a single speaker, a stereo pair, or
// a multi-speaker grouped zone — they all present as one Zone).
type Zone struct {
	Name          string
	CoordinatorIP string
	UUID          string // RINCON_xxx identifier of the coordinator
}

// ListSonosZones discovers Sonos speakers on the LAN via SSDP and returns
// the zones in the household. Set SONOS_HOST=<ip> to skip discovery (useful
// in VM/container networks where multicast is blocked).
func ListSonosZones(ctx context.Context) ([]Zone, error) {
	state, err := loadZoneGroupState(ctx)
	if err != nil {
		return nil, err
	}
	zones := make([]Zone, 0, len(state))
	for _, g := range state {
		var name, coordIP, uuid string
		for _, m := range g.Members {
			if m.UUID == g.Coordinator {
				name = m.ZoneName
				coordIP = ipFromLocation(m.Location)
				uuid = m.UUID
				break
			}
		}
		if name == "" {
			continue
		}
		zones = append(zones, Zone{Name: name, CoordinatorIP: coordIP, UUID: uuid})
	}
	return zones, nil
}

// PlayViaSonos resolves the query to a Spotify track URI (via the same
// authed Spotify client as Play, when free-text search is needed), then
// plays it on the Sonos zone matching `room` using Sonos's native Spotify
// integration over UPnP/SOAP.
//
// Pass WithContinue() to also queue the artist's top tracks after the seed.
//
// Env vars:
//
//	SONOS_HOST  optional; an IP of any speaker in the household, used to
//	            skip SSDP discovery if multicast is blocked
//	SONOS_SN    optional; the Spotify "service number" in Sonos. Default 1.
//	            If playback fails with a generic error, try 2 or 3.
func PlayViaSonos(ctx context.Context, query, room string, opts ...PlayOption) error {
	var o playOpts
	for _, fn := range opts {
		fn(&o)
	}

	info, err := resolveTrack(ctx, query)
	if err != nil {
		return err
	}

	zones, err := ListSonosZones(ctx)
	if err != nil {
		return err
	}
	needle := strings.ToLower(room)
	var zone *Zone
	for i := range zones {
		if strings.Contains(strings.ToLower(zones[i].Name), needle) {
			zone = &zones[i]
			break
		}
	}
	if zone == nil {
		names := make([]string, len(zones))
		for i, z := range zones {
			names[i] = z.Name
		}
		return fmt.Errorf("no Sonos zone matching %q; visible zones: %v", room, names)
	}

	cfg, err := lookupSpotifyService(ctx, zone.CoordinatorIP)
	if err != nil {
		return err
	}

	if !o.continueAfter {
		if err := playSpotifyOnSonos(ctx, zone.CoordinatorIP, info, cfg); err != nil {
			return err
		}
		_ = CacheTrack(ctx, info, -1)
		logEvent(ctx, EventRow{
			Kind:     "play_started",
			TrackID:  nullable(info.TrackID),
			ArtistID: nullable(info.ArtistID),
			Zone:     nullable(zone.Name),
			Source:   nullable("play"),
			Payload: marshalPayload(map[string]any{
				"transport":    "sonos",
				"continuation": false,
				"queue_size":   1,
			}),
		})
		return nil
	}

	more, err := gatherContinuationTracks(ctx, info, o.shuffle)
	if err != nil {
		return err
	}
	tracks := append([]*trackInfo{info}, more...)
	if err := playQueueOnSonos(ctx, zone, tracks, cfg); err != nil {
		return err
	}
	_ = CacheTrack(ctx, info, -1)
	logEvent(ctx, EventRow{
		Kind:     "play_started",
		TrackID:  nullable(info.TrackID),
		ArtistID: nullable(info.ArtistID),
		Zone:     nullable(zone.Name),
		Source:   nullable("play"),
		Payload: marshalPayload(map[string]any{
			"transport":    "sonos",
			"continuation": true,
			"shuffle":      o.shuffle,
			"queue_size":   len(tracks),
		}),
	})
	return nil
}

// resolveSonosZone discovers Sonos speakers and returns the matched zone's
// human-readable name and coordinator IP. Used by every transport-control
// helper below.
func resolveSonosZone(ctx context.Context, room string) (name, ip string, err error) {
	zones, err := ListSonosZones(ctx)
	if err != nil {
		return "", "", err
	}
	needle := strings.ToLower(room)
	for _, z := range zones {
		if strings.Contains(strings.ToLower(z.Name), needle) {
			return z.Name, z.CoordinatorIP, nil
		}
	}
	return "", "", fmt.Errorf("no Sonos zone matching %q", room)
}

// Pause pauses playback on the Sonos zone matching `room`.
func Pause(ctx context.Context, room string) error {
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return err
	}
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Pause xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID></u:Pause></s:Body>
</s:Envelope>`
	if _, err = soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#Pause", body); err != nil {
		return err
	}
	logEvent(ctx, EventRow{
		Kind:    "pause",
		Zone:    nullable(name),
		Source:  nullable("pause"),
		Payload: marshalPayload(map[string]any{}),
	})
	return nil
}

// Resume resumes playback on the Sonos zone matching `room`. Whatever was
// previously loaded continues from its current position.
func Resume(ctx context.Context, room string) error {
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return err
	}
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Play xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID><Speed>1</Speed></u:Play></s:Body>
</s:Envelope>`
	if _, err = soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#Play", body); err != nil {
		return err
	}
	logEvent(ctx, EventRow{
		Kind:    "resume",
		Zone:    nullable(name),
		Source:  nullable("resume"),
		Payload: marshalPayload(map[string]any{}),
	})
	return nil
}

// Stop stops playback on the Sonos zone matching `room`. Unlike Pause, this
// clears the current position — Resume after Stop restarts from 00:00.
func Stop(ctx context.Context, room string) error {
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return err
	}
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Stop xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID></u:Stop></s:Body>
</s:Envelope>`
	if _, err = soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#Stop", body); err != nil {
		return err
	}
	logEvent(ctx, EventRow{
		Kind:    "stop",
		Zone:    nullable(name),
		Source:  nullable("stop"),
		Payload: marshalPayload(map[string]any{}),
	})
	return nil
}

// Next advances to the next track in the Sonos zone's queue.
// SetShuffle enables or disables shuffle playback on a Sonos zone.
// Sonos's SHUFFLE_NOREPEAT mode randomizes the playback order across the
// queue without physically reordering it — turning shuffle off restores
// sequential playback from the same items.
func SetShuffle(ctx context.Context, room string, on bool) error {
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return err
	}
	mode := "NORMAL"
	if on {
		mode = "SHUFFLE_NOREPEAT"
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:SetPlayMode xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID><NewPlayMode>%s</NewPlayMode></u:SetPlayMode></s:Body>
</s:Envelope>`, mode)
	if _, err = soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#SetPlayMode", body); err != nil {
		return err
	}
	logEvent(ctx, EventRow{
		Kind:    "shuffle",
		Zone:    nullable(name),
		Source:  nullable("shuffle"),
		Payload: marshalPayload(map[string]any{"on": on}),
	})
	return nil
}

func Next(ctx context.Context, room string) error {
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return err
	}
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Next xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID></u:Next></s:Body>
</s:Envelope>`
	if _, err = soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#Next", body); err != nil {
		return err
	}
	// Track ID is unknown at this point — the zone is mid-transition and
	// GetPositionInfo would race. Downstream queries that want the skipped
	// track ID can correlate against the most recent play_started for the
	// same zone.
	logEvent(ctx, EventRow{
		Kind:    "skip",
		Zone:    nullable(name),
		Source:  nullable("next"),
		Payload: marshalPayload(map[string]any{}),
	})
	return nil
}

// Volume returns the current volume (0-100) of the Sonos zone matching `room`.
func Volume(ctx context.Context, room string) (int, error) {
	_, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return 0, err
	}
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:GetVolume xmlns:u="urn:schemas-upnp-org:service:RenderingControl:1">
<InstanceID>0</InstanceID><Channel>Master</Channel></u:GetVolume></s:Body>
</s:Envelope>`
	raw, err := soapCall(ctx, ip, "/MediaRenderer/RenderingControl/Control",
		"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume", body)
	if err != nil {
		return 0, err
	}
	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Resp struct {
				CurrentVolume int `xml:"CurrentVolume"`
			} `xml:"GetVolumeResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("parse GetVolume: %w", err)
	}
	return env.Body.Resp.CurrentVolume, nil
}

// SetVolume sets the volume of the Sonos zone matching `room` to `level`
// (clamped to 0-100).
func SetVolume(ctx context.Context, room string, level int) error {
	if level < 0 {
		level = 0
	}
	if level > 100 {
		level = 100
	}
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:SetVolume xmlns:u="urn:schemas-upnp-org:service:RenderingControl:1">
<InstanceID>0</InstanceID><Channel>Master</Channel><DesiredVolume>%d</DesiredVolume></u:SetVolume></s:Body>
</s:Envelope>`, level)
	if _, err = soapCall(ctx, ip, "/MediaRenderer/RenderingControl/Control",
		"urn:schemas-upnp-org:service:RenderingControl:1#SetVolume", body); err != nil {
		return err
	}
	logEvent(ctx, EventRow{
		Kind:    "volume",
		Zone:    nullable(name),
		Source:  nullable("vol"),
		Payload: marshalPayload(map[string]any{"level": level}),
	})
	return nil
}

// CurrentTrack describes what a Sonos zone is currently playing. Used for
// diagnosing playback URI/metadata format issues — drive Spotify on the
// zone via the Sonos app, then call CurrentTrack to see exactly what URI
// and DIDL-Lite metadata Sonos itself generates.
type CurrentTrack struct {
	Zone         string
	IP           string
	URI          string
	MetadataXML  string // raw, already-unescaped DIDL-Lite XML
}

// CurrentTrackFor returns the URI and DIDL-Lite metadata of whatever the
// Sonos zone matching `room` is playing right now.
func CurrentTrackFor(ctx context.Context, room string) (*CurrentTrack, error) {
	zones, err := ListSonosZones(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(room)
	for _, z := range zones {
		if !strings.Contains(strings.ToLower(z.Name), needle) {
			continue
		}
		pi, err := getPositionInfo(ctx, z.CoordinatorIP)
		if err != nil {
			return nil, err
		}
		return &CurrentTrack{Zone: z.Name, IP: z.CoordinatorIP, URI: pi.URI, MetadataXML: pi.Metadata}, nil
	}
	return nil, fmt.Errorf("no Sonos zone matching %q", room)
}

type positionInfo struct {
	URI      string
	Metadata string
	Position string // hh:mm:ss
	Duration string // hh:mm:ss; "0:00:00" for streams
	Track    int    // 1-based position in the queue; 0 if not playing from queue
}

func getPositionInfo(ctx context.Context, ip string) (*positionInfo, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:GetPositionInfo xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
</u:GetPositionInfo></s:Body>
</s:Envelope>`

	raw, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#GetPositionInfo", body)
	if err != nil {
		return nil, err
	}

	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Resp struct {
				Track         int    `xml:"Track"`
				TrackURI      string `xml:"TrackURI"`
				TrackMetaData string `xml:"TrackMetaData"`
				TrackDuration string `xml:"TrackDuration"`
				RelTime       string `xml:"RelTime"`
			} `xml:"GetPositionInfoResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse GetPositionInfo: %w", err)
	}
	return &positionInfo{
		URI:      env.Body.Resp.TrackURI,
		Metadata: env.Body.Resp.TrackMetaData,
		Position: env.Body.Resp.RelTime,
		Duration: env.Body.Resp.TrackDuration,
		Track:    env.Body.Resp.Track,
	}, nil
}

func getTransportInfo(ctx context.Context, ip string) (state string, err error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:GetTransportInfo xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
</u:GetTransportInfo></s:Body>
</s:Envelope>`

	raw, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo", body)
	if err != nil {
		return "", err
	}

	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Resp struct {
				CurrentTransportState string `xml:"CurrentTransportState"`
			} `xml:"GetTransportInfoResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("parse GetTransportInfo: %w", err)
	}
	return env.Body.Resp.CurrentTransportState, nil
}

// Playback summarises what a Sonos zone is currently doing. State values
// are Sonos-native: PLAYING, PAUSED_PLAYBACK, STOPPED, TRANSITIONING.
type Playback struct {
	Zone        string
	State       string
	Title       string
	Artist      string
	Album       string
	Duration    string // hh:mm:ss; empty for streams
	Position    string // hh:mm:ss
	URI         string
	AlbumArtURI string
}

type didlLite struct {
	XMLName xml.Name `xml:"DIDL-Lite"`
	Item    struct {
		Title       string `xml:"http://purl.org/dc/elements/1.1/ title"`
		Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
		Album       string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ album"`
		AlbumArtURI string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ albumArtURI"`
	} `xml:"item"`
}

// NowPlaying returns the current playback state for the Sonos zone matching
// `room`. Combines GetTransportInfo (state) and GetPositionInfo (track and
// position), then parses the DIDL-Lite metadata for title/artist/album.
func NowPlaying(ctx context.Context, room string) (*Playback, error) {
	zones, err := ListSonosZones(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(room)
	var ip, name string
	for _, z := range zones {
		if strings.Contains(strings.ToLower(z.Name), needle) {
			ip = z.CoordinatorIP
			name = z.Name
			break
		}
	}
	if ip == "" {
		return nil, fmt.Errorf("no Sonos zone matching %q", room)
	}

	state, err := getTransportInfo(ctx, ip)
	if err != nil {
		return nil, err
	}
	pi, err := getPositionInfo(ctx, ip)
	if err != nil {
		return nil, err
	}

	p := &Playback{
		Zone: name, State: state,
		URI: pi.URI, Position: pi.Position, Duration: pi.Duration,
	}
	if pi.Metadata != "" {
		var d didlLite
		if err := xml.Unmarshal([]byte(pi.Metadata), &d); err == nil {
			p.Title = d.Item.Title
			p.Artist = d.Item.Creator
			p.Album = d.Item.Album
			p.AlbumArtURI = d.Item.AlbumArtURI
		}
	}

	// Resolve the Spotify track ID once — it drives both metadata
	// enrichment (when Sonos's DIDL is empty for SMAPI items) and the
	// queue-advance detection below.
	trackID := extractSpotifyTrackID(p.URI)

	// Best-effort: look up artist/title/album. Prefer the local cache
	// (free) and fall back to the Spotify Web API on miss.
	var info *trackInfo
	if trackID != "" {
		if cached, err := LookupTrack(ctx, trackID); err == nil && cached != nil {
			info = cached
		} else if enriched, err := lookupSpotifyTracks(ctx, []string{trackID}); err == nil {
			if x, ok := enriched[trackID]; ok {
				info = x
				_ = CacheTrack(ctx, info, -1) // populate cache for future advances
			}
		}
	}
	if p.Title == "" && info != nil {
		p.Title = info.Title
		p.Artist = info.Artist
		p.Album = info.Album
	}

	// If the queue has advanced since the last event we recorded for
	// this zone, synthesize a play_started row (source="queue-advance")
	// so spot history / spot stats see auto-advanced tracks. Without
	// this, only the seed track of `play -c` ever lands in the events
	// table — Sonos drives queue progression on its own and our process
	// is long gone.
	if p.State == "PLAYING" && trackID != "" {
		artistID := ""
		if info != nil {
			artistID = info.ArtistID
		}
		logQueueAdvance(ctx, name, trackID, artistID)
	}

	// Best-effort observe event for the local-memory layer. Never fails
	// the read.
	writeObserveEvent(ctx, p, "now")
	return p, nil
}

// writeObserveEvent records a kind="observe" event when the zone is
// playing a Spotify track. Errors are swallowed — observation must never
// fail the caller's primary read.
func writeObserveEvent(ctx context.Context, p *Playback, source string) {
	if p == nil || p.State != "PLAYING" {
		return
	}
	id := extractSpotifyTrackID(p.URI)
	if id == "" {
		return
	}
	payload := map[string]any{
		"position_ms": parseSonosTimeMS(p.Position),
		"track_id":    id,
		"state":       p.State,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = InsertEvent(ctx, EventRow{
		Kind:       "observe",
		TrackID:    sql.NullString{String: id, Valid: true},
		Zone:       sql.NullString{String: p.Zone, Valid: p.Zone != ""},
		Source:     sql.NullString{String: source, Valid: true},
		Payload:    sql.NullString{String: string(buf), Valid: true},
		OccurredAt: time.Now().Unix(),
	})
}

// parseSonosTimeMS converts a Sonos hh:mm:ss timestamp to milliseconds.
// Returns 0 on parse failure or empty input.
func parseSonosTimeMS(s string) int64 {
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0
	}
	return int64(h*3600+m*60+sec) * 1000
}

// SpotifyOnSonosInfo returns the Spotify service registration as Sonos sees
// it for the given zone. Useful for diagnosing playback errors.
type SpotifyOnSonosInfo struct {
	Zone string
	IP   string
	SID  int // service ID in URI's sid= param
	Type int // service type in DIDL-Lite metadata token
	SN   int // user's Spotify account index in Sonos (env-overridable)
}

// SpotifyServiceFor looks up Spotify's SMAPI registration on the Sonos zone
// matching `room`. Use it from `spot diag` or from harness code that wants
// to log the values it'll use for playback.
func SpotifyServiceFor(ctx context.Context, room string) (*SpotifyOnSonosInfo, error) {
	zones, err := ListSonosZones(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(room)
	for _, z := range zones {
		if !strings.Contains(strings.ToLower(z.Name), needle) {
			continue
		}
		cfg, err := lookupSpotifyService(ctx, z.CoordinatorIP)
		if err != nil {
			return nil, err
		}
		return &SpotifyOnSonosInfo{Zone: z.Name, IP: z.CoordinatorIP, SID: cfg.sid, Type: cfg.typ, SN: cfg.sn}, nil
	}
	return nil, fmt.Errorf("no Sonos zone matching %q", room)
}

// playQueueOnSonos clears the zone's playback queue, enqueues the supplied
// tracks in order (seed first, then continuation), points the AVTransport
// at the queue, and starts playing. Used by PlayViaSonos when
// WithContinue() is set.
func playQueueOnSonos(ctx context.Context, zone *Zone, tracks []*trackInfo, cfg *spotifyOnSonos) error {
	ip := zone.CoordinatorIP
	if err := stopAVT(ctx, ip); err != nil {
		// Best-effort; an empty/idle AVTransport returns an error we can ignore.
		_ = err
	}
	if err := clearQueue(ctx, ip); err != nil {
		return fmt.Errorf("clear queue: %w", err)
	}
	for _, t := range tracks {
		if _, err := addURIToQueue(ctx, ip, t, cfg); err != nil {
			return fmt.Errorf("queue %s: %w", t.TrackID, err)
		}
	}

	queueURI := fmt.Sprintf("x-rincon-queue:%s#0", zone.UUID)
	setBody := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
<CurrentURI>%s</CurrentURI>
<CurrentURIMetaData></CurrentURIMetaData>
</u:SetAVTransportURI></s:Body>
</s:Envelope>`, escapeXML(queueURI))
	if _, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#SetAVTransportURI", setBody); err != nil {
		return fmt.Errorf("set queue as transport: %w", err)
	}

	playBody := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Play xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID><Speed>1</Speed></u:Play></s:Body>
</s:Envelope>`
	if _, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#Play", playBody); err != nil {
		return fmt.Errorf("play queue: %w", err)
	}
	return nil
}

func stopAVT(ctx context.Context, ip string) error {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Stop xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID></u:Stop></s:Body>
</s:Envelope>`
	_, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#Stop", body)
	return err
}

func clearQueue(ctx context.Context, ip string) error {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:RemoveAllTracksFromQueue xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID></u:RemoveAllTracksFromQueue></s:Body>
</s:Envelope>`
	_, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#RemoveAllTracksFromQueue", body)
	return err
}

type queueAddResult struct {
	Position  int // FirstTrackNumberEnqueued — 1-based slot the track landed in
	NewLength int // total tracks in the queue after this add
}

func addURIToQueue(ctx context.Context, ip string, info *trackInfo, cfg *spotifyOnSonos) (*queueAddResult, error) {
	uri := spotifySonosURI(info.TrackID, cfg)
	meta := spotifySonosMetadata(info, cfg)

	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:AddURIToQueue xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
<EnqueuedURI>%s</EnqueuedURI>
<EnqueuedURIMetaData>%s</EnqueuedURIMetaData>
<DesiredFirstTrackNumberEnqueued>0</DesiredFirstTrackNumberEnqueued>
<EnqueueAsNext>0</EnqueueAsNext>
</u:AddURIToQueue></s:Body>
</s:Envelope>`, escapeXML(uri), escapeXML(meta))

	raw, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#AddURIToQueue", body)
	if err != nil {
		return nil, err
	}

	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Resp struct {
				FirstTrackNumberEnqueued int `xml:"FirstTrackNumberEnqueued"`
				NewQueueLength           int `xml:"NewQueueLength"`
			} `xml:"AddURIToQueueResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse AddURIToQueue: %w", err)
	}
	return &queueAddResult{
		Position:  env.Body.Resp.FirstTrackNumberEnqueued,
		NewLength: env.Body.Resp.NewQueueLength,
	}, nil
}

// EnqueueResult reports where a track landed in the Sonos queue and the
// queue's new length.
type EnqueueResult struct {
	Zone      string
	Position  int
	NewLength int
}

// EnqueueViaSonos appends a track to the Sonos zone's playback queue. Does
// not change transport state — if the zone isn't currently playing from the
// queue, you'll need to start playback (e.g. `spot play -v sonos -c …`) for
// the queued tracks to actually play.
func EnqueueViaSonos(ctx context.Context, query, room string) (*EnqueueResult, error) {
	info, err := resolveTrack(ctx, query)
	if err != nil {
		return nil, err
	}
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return nil, err
	}
	cfg, err := lookupSpotifyService(ctx, ip)
	if err != nil {
		return nil, err
	}
	res, err := addURIToQueue(ctx, ip, info, cfg)
	if err != nil {
		return nil, err
	}
	_ = CacheTrack(ctx, info, -1)
	logEvent(ctx, EventRow{
		Kind:     "enqueue",
		TrackID:  nullable(info.TrackID),
		ArtistID: nullable(info.ArtistID),
		Zone:     nullable(name),
		Source:   nullable("enqueue"),
		Payload: marshalPayload(map[string]any{
			"position":   res.Position,
			"new_length": res.NewLength,
		}),
	})
	return &EnqueueResult{Zone: name, Position: res.Position, NewLength: res.NewLength}, nil
}

// QueueTrack is one entry in a Sonos zone's playback queue.
type QueueTrack struct {
	Title  string
	Artist string
	Album  string
	URI    string
}

// QueueListing is the playback queue plus the currently-playing position.
type QueueListing struct {
	Zone     string
	Position int          // 1-based; 0 if the zone isn't playing from the queue
	Tracks   []QueueTrack // queue contents in order
}

// ShowQueue returns the playback queue for the Sonos zone matching `room`,
// plus the current position within it (if the zone is playing from the queue).
func ShowQueue(ctx context.Context, room string) (*QueueListing, error) {
	name, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return nil, err
	}
	tracks, err := browseQueue(ctx, ip)
	if err != nil {
		return nil, err
	}

	enrichQueueFromSpotify(ctx, tracks)

	pos := 0
	// CurrentURI from GetMediaInfo is the transport URI (queue, stream, etc).
	// TrackURI from GetPositionInfo is the current track within that transport.
	// For "is the queue active?" we want the former.
	if mi, err := getMediaInfo(ctx, ip); err == nil && strings.HasPrefix(mi.CurrentURI, "x-rincon-queue:") {
		if pi, err := getPositionInfo(ctx, ip); err == nil {
			pos = pi.Track
			// Best-effort observe event — only if the zone is playing.
			if state, err := getTransportInfo(ctx, ip); err == nil {
				writeObserveEvent(ctx, &Playback{
					Zone:     name,
					State:    state,
					URI:      pi.URI,
					Position: pi.Position,
					Duration: pi.Duration,
				}, "queue")
			}
		}
	}
	return &QueueListing{Zone: name, Position: pos, Tracks: tracks}, nil
}

// extractSpotifyTrackID pulls a Spotify track ID out of a Sonos x-sonos-spotify
// URI. Returns "" for non-Spotify URIs.
func extractSpotifyTrackID(uri string) string {
	const prefix = "x-sonos-spotify:spotify:track:"
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(uri, prefix)
	if i := strings.Index(rest, "?"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// enrichQueueFromSpotify fills in Title/Artist/Album for queue tracks that
// have no metadata, by looking them up via the Spotify Web API. Sonos
// strips DIDL metadata for SMAPI queue items and only stores a URI stub,
// so the only reliable way to display useful labels is to re-fetch from
// Spotify on read. Best-effort — silently leaves fields blank if Spotify
// auth is unavailable or lookup fails.
func enrichQueueFromSpotify(ctx context.Context, tracks []QueueTrack) {
	var ids []string
	for _, t := range tracks {
		if t.Title == "" {
			if id := extractSpotifyTrackID(t.URI); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	enriched, err := lookupSpotifyTracks(ctx, ids)
	if err != nil {
		return
	}
	for i := range tracks {
		if tracks[i].Title != "" {
			continue
		}
		id := extractSpotifyTrackID(tracks[i].URI)
		if id == "" {
			continue
		}
		if info, ok := enriched[id]; ok {
			tracks[i].Title = info.Title
			tracks[i].Artist = info.Artist
			tracks[i].Album = info.Album
		}
	}
}

// BrowseQueueRaw returns the raw DIDL-Lite XML of the zone's queue, as Sonos
// returns it. Useful for diagnostic commands that want to display the exact
// metadata Sonos has stored (or hasn't, as the case may be).
func BrowseQueueRaw(ctx context.Context, room string) (string, error) {
	_, ip, err := resolveSonosZone(ctx, room)
	if err != nil {
		return "", err
	}
	return browseQueueRaw(ctx, ip)
}

type mediaInfo struct {
	CurrentURI         string
	CurrentURIMetaData string
	NrTracks           int
}

func getMediaInfo(ctx context.Context, ip string) (*mediaInfo, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:GetMediaInfo xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
</u:GetMediaInfo></s:Body>
</s:Envelope>`
	raw, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#GetMediaInfo", body)
	if err != nil {
		return nil, err
	}
	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Resp struct {
				NrTracks           int    `xml:"NrTracks"`
				CurrentURI         string `xml:"CurrentURI"`
				CurrentURIMetaData string `xml:"CurrentURIMetaData"`
			} `xml:"GetMediaInfoResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse GetMediaInfo: %w", err)
	}
	return &mediaInfo{
		CurrentURI:         env.Body.Resp.CurrentURI,
		CurrentURIMetaData: env.Body.Resp.CurrentURIMetaData,
		NrTracks:           env.Body.Resp.NrTracks,
	}, nil
}

func browseQueueRaw(ctx context.Context, ip string) (string, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">
<ObjectID>Q:0</ObjectID>
<BrowseFlag>BrowseDirectChildren</BrowseFlag>
<Filter>*</Filter>
<StartingIndex>0</StartingIndex>
<RequestedCount>1000</RequestedCount>
<SortCriteria></SortCriteria>
</u:Browse></s:Body>
</s:Envelope>`

	raw, err := soapCall(ctx, ip, "/MediaServer/ContentDirectory/Control",
		"urn:schemas-upnp-org:service:ContentDirectory:1#Browse", body)
	if err != nil {
		return "", err
	}
	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Resp struct {
				Result string `xml:"Result"`
			} `xml:"BrowseResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("parse Browse: %w", err)
	}
	return env.Body.Resp.Result, nil
}

func browseQueue(ctx context.Context, ip string) ([]QueueTrack, error) {
	result, err := browseQueueRaw(ctx, ip)
	if err != nil {
		return nil, err
	}

	// Local-name-only tags so namespace-prefixed children (dc:title,
	// upnp:album) match. Go's encoding/xml has unmarshal quirks with
	// "ns localname" tags when the document declares prefixes; the
	// docs say no-namespace tags match any namespace on unmarshal.
	var didl struct {
		XMLName xml.Name `xml:"DIDL-Lite"`
		Items   []struct {
			Title   string `xml:"title"`
			Creator string `xml:"creator"`
			Album   string `xml:"album"`
			Res     string `xml:"res"`
		} `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(result), &didl); err != nil {
		return nil, fmt.Errorf("parse queue DIDL: %w", err)
	}

	tracks := make([]QueueTrack, len(didl.Items))
	for i, item := range didl.Items {
		tracks[i] = QueueTrack{
			Title:  item.Title,
			Artist: item.Creator,
			Album:  item.Album,
			URI:    item.Res,
		}
	}
	return tracks, nil
}

// ---- discovery ------------------------------------------------------------

func loadZoneGroupState(ctx context.Context) ([]zoneGroup, error) {
	var anyIP string
	if v := os.Getenv("SONOS_HOST"); v != "" {
		anyIP = v
	} else {
		ips, err := discoverSonos(ctx, discoveryFor)
		if err != nil {
			return nil, fmt.Errorf("ssdp discovery: %w", err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no Sonos speakers found via SSDP — set SONOS_HOST=<ip> to bypass discovery if multicast is blocked")
		}
		anyIP = ips[0]
	}
	return getZoneGroupState(ctx, anyIP)
}

func discoverSonos(ctx context.Context, timeout time.Duration) ([]string, error) {
	dst, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: " + sonosSearch + "\r\n\r\n"
	if _, err := conn.WriteTo([]byte(msg), dst); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	seen := map[string]bool{}
	var ips []string
	buf := make([]byte, 4096)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		body := string(buf[:n])
		if !strings.Contains(body, "Sonos") && !strings.Contains(body, sonosSearch) {
			continue
		}
		ip := src.IP.String()
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

func ipFromLocation(loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	h, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return u.Host
	}
	return h
}

// ---- ZoneGroupState parsing ----------------------------------------------

type zgsEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		GetZoneGroupStateResponse struct {
			ZoneGroupState string `xml:"ZoneGroupState"`
		}
	} `xml:"Body"`
}

type zoneGroupsXML struct {
	XMLName xml.Name    `xml:"ZoneGroupState"`
	Groups  []zoneGroup `xml:"ZoneGroups>ZoneGroup"`
}

type zoneGroup struct {
	Coordinator string            `xml:"Coordinator,attr"`
	ID          string            `xml:"ID,attr"`
	Members     []zoneGroupMember `xml:"ZoneGroupMember"`
}

type zoneGroupMember struct {
	UUID     string `xml:"UUID,attr"`
	Location string `xml:"Location,attr"`
	ZoneName string `xml:"ZoneName,attr"`
}

func getZoneGroupState(ctx context.Context, ip string) ([]zoneGroup, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:GetZoneGroupState xmlns:u="urn:schemas-upnp-org:service:ZoneGroupTopology:1"></u:GetZoneGroupState></s:Body>
</s:Envelope>`

	raw, err := soapCall(ctx, ip, "/ZoneGroupTopology/Control",
		"urn:schemas-upnp-org:service:ZoneGroupTopology:1#GetZoneGroupState", body)
	if err != nil {
		return nil, err
	}

	var env zgsEnvelope
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse soap envelope: %w", err)
	}
	var zgs zoneGroupsXML
	if err := xml.Unmarshal([]byte(env.Body.GetZoneGroupStateResponse.ZoneGroupState), &zgs); err != nil {
		return nil, fmt.Errorf("parse zone groups: %w", err)
	}
	return zgs.Groups, nil
}

// ---- service registration lookup -----------------------------------------
//
// Sonos doesn't speak Spotify directly — it routes via the SMAPI music-service
// framework. Each linked service gets:
//
//	Id   (sid)  — short numeric ID the Spotify service exposes (sid=9 for
//	              Spotify on essentially every household, but we read it
//	              rather than hardcode in case a future migration changes it)
//	Type        — type number used in the DIDL-Lite metadata token, e.g.
//	              2311 (legacy SMAPI v1) or 3079 (SMAPI v2). This is the
//	              one that varies between accounts depending on when the
//	              Sonos↔Spotify link was established.
//	Sn          — account/serial number of the user's Spotify login within
//	              Sonos. Almost always 1 for one-Spotify-account households.
//	              Override with SONOS_SN env var if needed.

type spotifyOnSonos struct {
	sid  int
	typ  int
	sn   int
}

type listServicesEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Resp struct {
			Descriptor string `xml:"AvailableServiceDescriptorList"`
			Types      string `xml:"AvailableServiceTypeList"`
		} `xml:"ListAvailableServicesResponse"`
	} `xml:"Body"`
}

type smapiService struct {
	ID   int    `xml:"Id,attr"`
	Name string `xml:"Name,attr"`
}

func lookupSpotifyService(ctx context.Context, ip string) (*spotifyOnSonos, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:ListAvailableServices xmlns:u="urn:schemas-upnp-org:service:MusicServices:1"></u:ListAvailableServices></s:Body>
</s:Envelope>`

	raw, err := soapCall(ctx, ip, "/MusicServices/Control",
		"urn:schemas-upnp-org:service:MusicServices:1#ListAvailableServices", body)
	if err != nil {
		return nil, err
	}

	var env listServicesEnvelope
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse ListAvailableServices envelope: %w", err)
	}

	type servicesXML struct {
		Service []smapiService `xml:"Service"`
	}
	var svcs servicesXML
	if err := xml.Unmarshal([]byte(env.Body.Resp.Descriptor), &svcs); err != nil {
		return nil, fmt.Errorf("parse service descriptor: %w", err)
	}

	rawTypes := strings.Split(env.Body.Resp.Types, ",")
	types := make([]int, 0, len(rawTypes))
	for _, t := range rawTypes {
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err == nil {
			types = append(types, n)
		}
	}

	for i, s := range svcs.Service {
		if !strings.EqualFold(s.Name, "Spotify") {
			continue
		}
		cfg := &spotifyOnSonos{sid: s.ID, sn: 1}
		if i < len(types) {
			cfg.typ = types[i]
		}
		if v := os.Getenv("SONOS_SN"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				cfg.sn = n
			}
		}
		return cfg, nil
	}

	names := make([]string, len(svcs.Service))
	for i, s := range svcs.Service {
		names[i] = s.Name
	}
	return nil, fmt.Errorf("Spotify is not linked in Sonos for this household (services found: %v)", names)
}

// ---- playback ------------------------------------------------------------

// spotifySonosURI constructs the x-sonos-spotify URI Sonos expects for a
// track in SMAPI v2 mode (sid=12, flags=0, plain colons in the path).
func spotifySonosURI(trackID string, cfg *spotifyOnSonos) string {
	return fmt.Sprintf("x-sonos-spotify:spotify:track:%s?sid=%d&flags=0&sn=%d",
		trackID, cfg.sid, cfg.sn)
}

// spotifySonosMetadata builds the DIDL-Lite blob Sonos stores for a track.
//
// Two pieces are non-obvious and required for Sonos to keep the metadata
// (rather than discarding it and synthesising a generic stub):
//
//  1. id="00030020spotify%3atrack%3a<TRACKID>" — Sonos's required item-ID
//     format for Spotify tracks. With anything else (e.g. id="-1"), Sonos
//     throws the metadata away and stores only a minimal <res>+albumArt
//     stub keyed by Q:0/N.
//  2. <desc id="cdudn">SA_RINCON<type>_X_#Svc<type>-0-Token</desc> — the
//     service-registration identifier so Sonos knows which SMAPI account
//     the item belongs to. <type> comes from lookupSpotifyService.
func spotifySonosMetadata(info *trackInfo, cfg *spotifyOnSonos) string {
	itemID := "00030020spotify%3atrack%3a" + info.TrackID
	uri := spotifySonosURI(info.TrackID, cfg)
	return fmt.Sprintf(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:r="urn:schemas-rinconnetworks-com:metadata-1-0/" xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"><item id="%s" parentID="-1" restricted="true"><res>%s</res><dc:title>%s</dc:title><dc:creator>%s</dc:creator><upnp:album>%s</upnp:album><upnp:class>object.item.audioItem.musicTrack</upnp:class><desc id="cdudn" nameSpace="urn:schemas-rinconnetworks-com:metadata-1-0/">SA_RINCON%d_X_#Svc%d-0-Token</desc></item></DIDL-Lite>`,
		itemID, uri,
		escapeXML(info.Title), escapeXML(info.Artist), escapeXML(info.Album),
		cfg.typ, cfg.typ)
}

func playSpotifyOnSonos(ctx context.Context, ip string, info *trackInfo, cfg *spotifyOnSonos) error {
	sonosURI := spotifySonosURI(info.TrackID, cfg)
	metadata := spotifySonosMetadata(info, cfg)

	setBody := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
<CurrentURI>%s</CurrentURI>
<CurrentURIMetaData>%s</CurrentURIMetaData>
</u:SetAVTransportURI></s:Body>
</s:Envelope>`, escapeXML(sonosURI), escapeXML(metadata))

	if _, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#SetAVTransportURI", setBody); err != nil {
		return fmt.Errorf("SetAVTransportURI: %w", err)
	}

	playBody := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:Play xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID><Speed>1</Speed>
</u:Play></s:Body>
</s:Envelope>`

	if _, err := soapCall(ctx, ip, "/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1#Play", playBody); err != nil {
		return fmt.Errorf("Play: %w", err)
	}
	return nil
}

func soapCall(ctx context.Context, ip, path, action, body string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://%s:%d%s", ip, sonosPort, path),
		strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+action+`"`)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sonos %s returned %d: %s", path, resp.StatusCode, string(raw))
	}
	return raw, nil
}

func escapeXML(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
