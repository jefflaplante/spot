// spot is a thin CLI wrapper around the spotify package, intended for an
// agent harness that prefers shelling out over linking Go code.
//
// Usage:
//
//	spot auth
//	spot devices                                                # Connect devices
//	spot zones                                                  # Sonos zones via UPnP
//	spot play "bonobo migration" "Living Room"                  # via Spotify Connect
//	spot play -v sonos "bonobo migration" "Parlor"              # via Sonos directly
//	spot play "spotify:track:4cOdK2wGLETKBW3PvgPWqT" "Kitchen"
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jefflaplante/spot"
	"github.com/spf13/cobra"
)

const defaultVolumeStep = 5

// jsonOutput is set by the persistent --json root flag and consumed by emit
// in output.go.
var jsonOutput bool

// trimZeroHour drops a leading "0:" from Sonos's hh:mm:ss timestamps so
// "0:03:13" displays as "03:13".
func trimZeroHour(t string) string {
	return strings.TrimPrefix(t, "0:")
}

// okResult is the JSON envelope returned by commands that have no other
// useful output on success (pause, resume, stop, next/skip, play).
type okResult struct {
	OK      bool   `json:"ok"`
	Command string `json:"command"`
	Zone    string `json:"zone,omitempty"`
}

// deviceJSON is the JSON shape used by `spot devices`.
type deviceJSON struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	ID         string `json:"id"`
	Active     bool   `json:"active"`
	Restricted bool   `json:"restricted"`
	Volume     int    `json:"volume,omitempty"`
}

// zoneJSON is the JSON shape used by `spot zones`.
type zoneJSON struct {
	Name          string `json:"name"`
	CoordinatorIP string `json:"coordinator_ip"`
	UUID          string `json:"uuid,omitempty"`
}

// volumeJSON is the JSON shape used by `spot vol` (both query and set).
type volumeJSON struct {
	Zone   string `json:"zone"`
	Volume int    `json:"volume"`
}

// queueRawJSON wraps the raw DIDL-Lite XML when `--raw --json` are combined.
type queueRawJSON struct {
	Zone string `json:"zone"`
	XML  string `json:"xml"`
}

// --- personalization JSON shapes (top, recent, liked, follows, playlists) ---

// topTrackJSON is the JSON shape used by `spot top tracks`.
type topTrackJSON struct {
	Rank       int    `json:"rank"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Popularity int    `json:"popularity"`
}

// topArtistJSON is the JSON shape used by `spot top artists`.
type topArtistJSON struct {
	Rank       int      `json:"rank"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Popularity int      `json:"popularity"`
	Genres     []string `json:"genres"`
}

// recentTrackJSON is the JSON shape used by `spot recent`.
type recentTrackJSON struct {
	PlayedAt string `json:"played_at"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
}

// likedTrackJSON is the JSON shape used by `spot liked`.
type likedTrackJSON struct {
	AddedAt string `json:"added_at"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
}

// followedArtistJSON is the JSON shape used by `spot follows`.
type followedArtistJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Popularity int      `json:"popularity"`
	Genres     []string `json:"genres"`
}

// playlistJSON is the JSON shape used by `spot playlists`.
type playlistJSON struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Tracks int    `json:"tracks"`
	Public bool   `json:"public"`
}

// --- library-mutation JSON shapes (like/unlike, follow/unfollow, playlist) ---

// libTrackJSON is the JSON shape used by `like` / `unlike` and as a nested
// field in `playlist add` / `playlist remove`.
type libTrackJSON struct {
	Action  string `json:"action,omitempty"`
	TrackID string `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
}

// followJSON is the JSON shape used by `follow` / `unfollow`.
type followJSON struct {
	Action   string `json:"action"`
	ArtistID string `json:"artist_id"`
	Name     string `json:"name"`
}

// playlistNewJSON is the JSON shape used by `playlist new`.
type playlistNewJSON struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Public bool   `json:"public"`
	URI    string `json:"uri"`
}

// playlistTrackJSON is a nested track summary inside playlistMutateJSON.
type playlistTrackJSON struct {
	TrackID string `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
}

// playlistMutateJSON is the JSON shape used by `playlist add` / `playlist remove`.
type playlistMutateJSON struct {
	Action   string            `json:"action"`
	Playlist string            `json:"playlist"`
	Track    playlistTrackJSON `json:"track"`
}

// freshAlbumJSON is the JSON shape used by `spot fresh`. One row per album
// with first-track-only TrackIDs by default; --all-tracks expands it.
type freshAlbumJSON struct {
	AlbumID     string   `json:"album_id"`
	AlbumName   string   `json:"album"`
	ArtistID    string   `json:"artist_id"`
	ArtistName  string   `json:"artist"`
	ReleaseDate string   `json:"release_date"`
	TrackIDs    []string `json:"track_ids"`
}

// --- discovery JSON shapes (mix, daily, deeper, rabbithole) ------------

// mixTrackJSON is the JSON shape for one row in `spot mix --json`.
type mixTrackJSON struct {
	Position int    `json:"position"`
	TrackID  string `json:"track_id"`
	Title    string `json:"title,omitempty"`
	Artist   string `json:"artist,omitempty"`
	Album    string `json:"album,omitempty"`
}

// mixResultJSON is the top-level JSON object for `spot mix`.
type mixResultJSON struct {
	Length int            `json:"length"`
	Seed   string         `json:"seed,omitempty"`
	Zone   string         `json:"zone,omitempty"`
	Tracks []mixTrackJSON `json:"tracks"`
}

// dailyTrackJSON is the JSON shape used by `spot daily`. One row per
// suggested track in the order Daily returns them (love-boosted first,
// then long-term rank).
type dailyTrackJSON struct {
	Rank    int    `json:"rank"`
	TrackID string `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Album   string `json:"album,omitempty"`
}

// deeperTrackJSON is the JSON shape used by `spot deeper`. One row per
// surfaced track, ordered oldest-album-first — the chronological walk
// through an artist's discography minus their top-10 hits.
type deeperTrackJSON struct {
	AlbumID     string `json:"album_id"`
	AlbumName   string `json:"album"`
	ReleaseDate string `json:"release_date"`
	TrackID     string `json:"track_id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
}

// rabbitholeTrackJSON is one chosen track in `spot rabbithole --json`.
type rabbitholeTrackJSON struct {
	TrackID string `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Album   string `json:"album,omitempty"`
}

// rabbitholeStepJSON mirrors spot.RabbitholeStep for the CLI's JSON shape;
// kept as its own type so we control the field ordering and tags.
type rabbitholeStepJSON struct {
	ArtistID     string   `json:"artist_id"`
	ArtistName   string   `json:"artist_name"`
	GenreOverlap int      `json:"genre_overlap"`
	TrackIDs     []string `json:"track_ids"`
}

// rabbitholeJSON is the envelope `spot rabbithole --json` emits: the
// chosen tracks plus the artist-by-artist walk that produced them. The
// walk is included now so a future --explain flag can render the
// reasoning without re-running the algorithm.
type rabbitholeJSON struct {
	Seed   string                `json:"seed,omitempty"`
	Zone   string                `json:"zone,omitempty"`
	Tracks []rabbitholeTrackJSON `json:"tracks"`
	Walk   []rabbitholeStepJSON  `json:"walk"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	root := &cobra.Command{
		Use:           "spot",
		Short:         "Play Spotify tracks on Sonos (or any Connect device) via the Web API",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON instead of human-readable output")

	root.AddCommand(&cobra.Command{
		Use:   "auth",
		Short: "Run the one-time OAuth bootstrap and write the token cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := spot.Authenticate(cmd.Context()); err != nil {
				return err
			}
			return emit(okResult{OK: true, Command: "auth"}, nil)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "devices",
		Short: "List playback devices currently visible to Spotify",
		Long: `Print every device Spotify can see for your account: name, type, ID,
whether it's the active device, and whether it's restricted (some Connect
endpoints accept commands but block volume changes, etc).

If a Sonos zone you expect is missing, open the Spotify app (not the Sonos
app), tap the Connect/devices icon, pick the zone, and play any track. The
zone will then register as a Spotify Connect device and appear here.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			devs, err := spot.Devices(cmd.Context())
			if err != nil {
				return err
			}
			out := make([]deviceJSON, 0, len(devs))
			for _, d := range devs {
				out = append(out, deviceJSON{
					Name:       d.Name,
					Type:       d.Type,
					ID:         string(d.ID),
					Active:     d.Active,
					Restricted: d.Restricted,
					Volume:     int(d.Volume),
				})
			}
			return emit(out, func() error {
				if len(devs) == 0 {
					fmt.Fprintln(os.Stderr, "no devices visible to Spotify")
					fmt.Fprintln(os.Stderr, "open the Spotify app, pick the Sonos zone via the Connect/devices icon, play any track, then retry")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tTYPE\tACTIVE\tRESTRICTED\tID")
				for _, d := range devs {
					fmt.Fprintf(tw, "%s\t%s\t%t\t%t\t%s\n", d.Name, d.Type, d.Active, d.Restricted, d.ID)
				}
				return tw.Flush()
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "zones",
		Short: "List Sonos zones discovered on the LAN via UPnP",
		Long: `Discover Sonos speakers via SSDP and print each zone's name and the
IP of its coordinator. Use this to verify the Sonos-direct path independently
of Spotify Connect.

Set SONOS_HOST=<ip> to skip SSDP discovery if multicast is blocked on your
network (common in some VM/container setups).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			zones, err := spot.ListSonosZones(cmd.Context())
			if err != nil {
				return err
			}
			out := make([]zoneJSON, 0, len(zones))
			for _, z := range zones {
				out = append(out, zoneJSON{Name: z.Name, CoordinatorIP: z.CoordinatorIP, UUID: z.UUID})
			}
			return emit(out, func() error {
				if len(zones) == 0 {
					fmt.Fprintln(os.Stderr, "no Sonos zones found")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ZONE\tCOORDINATOR")
				for _, z := range zones {
					fmt.Fprintf(tw, "%s\t%s\n", z.Name, z.CoordinatorIP)
				}
				return tw.Flush()
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "pause <zone>",
		Short: "Pause playback on a Sonos zone",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := spot.Pause(cmd.Context(), args[0]); err != nil {
				return err
			}
			return emit(okResult{OK: true, Command: "pause", Zone: args[0]}, nil)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume <zone>",
		Short: "Resume playback on a Sonos zone (continues from current position)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := spot.Resume(cmd.Context(), args[0]); err != nil {
				return err
			}
			return emit(okResult{OK: true, Command: "resume", Zone: args[0]}, nil)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "enqueue <query> <zone>",
		Short: "Append a track to a Sonos zone's playback queue",
		Long: `Search Spotify and append the top hit to the Sonos zone's playback queue.

Does not change transport state. If the zone isn't currently playing from
the queue, start it with "spot play -v sonos -c <something> <zone>" first
(or use the Sonos app), then enqueue subsequent tracks.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := spot.EnqueueViaSonos(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			out := struct {
				Zone      string `json:"zone"`
				Position  int    `json:"position"`
				NewLength int    `json:"new_length"`
			}{Zone: res.Zone, Position: res.Position, NewLength: res.NewLength}
			return emit(out, func() error {
				fmt.Printf("%s: queued at position %d (queue now has %d tracks)\n",
					res.Zone, res.Position, res.NewLength)
				return nil
			})
		},
	})

	var queueRaw bool
	queueCmd := &cobra.Command{
		Use:   "queue <zone>",
		Short: "Show the playback queue and current position",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if queueRaw {
				xmlStr, err := spot.BrowseQueueRaw(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return emit(queueRawJSON{Zone: args[0], XML: xmlStr}, func() error {
					fmt.Println(xmlStr)
					return nil
				})
			}
			q, err := spot.ShowQueue(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			type queueTrackJSON struct {
				Position int    `json:"position"`
				Title    string `json:"title"`
				Artist   string `json:"artist"`
				Album    string `json:"album"`
				URI      string `json:"uri"`
				Current  bool   `json:"current"`
			}
			tracks := make([]queueTrackJSON, 0, len(q.Tracks))
			for i, t := range q.Tracks {
				tracks = append(tracks, queueTrackJSON{
					Position: i + 1,
					Title:    t.Title,
					Artist:   t.Artist,
					Album:    t.Album,
					URI:      t.URI,
					Current:  i+1 == q.Position,
				})
			}
			out := struct {
				Zone     string           `json:"zone"`
				Position int              `json:"position"`
				Length   int              `json:"length"`
				Tracks   []queueTrackJSON `json:"tracks"`
			}{Zone: q.Zone, Position: q.Position, Length: len(q.Tracks), Tracks: tracks}
			return emit(out, func() error {
				if len(q.Tracks) == 0 {
					fmt.Printf("%s: queue is empty\n", q.Zone)
					return nil
				}
				if q.Position > 0 {
					fmt.Printf("%s — %d tracks, playing %d\n", q.Zone, len(q.Tracks), q.Position)
				} else {
					fmt.Printf("%s — %d tracks (zone not playing from queue)\n", q.Zone, len(q.Tracks))
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				for i, t := range q.Tracks {
					mark := " "
					if i+1 == q.Position {
						mark = ">"
					}
					label := t.Title
					if t.Artist != "" {
						label = t.Artist + " — " + t.Title
					}
					if label == "" {
						label = "(no metadata) " + t.URI
					}
					fmt.Fprintf(tw, "%s\t%d\t%s\n", mark, i+1, label)
				}
				return tw.Flush()
			})
		},
	}
	queueCmd.Flags().BoolVar(&queueRaw, "raw", false, "print the raw DIDL-Lite XML returned by Sonos for debugging")
	root.AddCommand(queueCmd)

	root.AddCommand(&cobra.Command{
		Use:     "next <zone>",
		Aliases: []string{"skip"},
		Short:   "Skip to the next track in the zone's queue",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := spot.Next(cmd.Context(), args[0]); err != nil {
				return err
			}
			return emit(okResult{OK: true, Command: "next", Zone: args[0]}, nil)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "stop <zone>",
		Short: "Stop playback on a Sonos zone (resets position to start)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := spot.Stop(cmd.Context(), args[0]); err != nil {
				return err
			}
			return emit(okResult{OK: true, Command: "stop", Zone: args[0]}, nil)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "vol <zone> [up|down|<level>] [step]",
		Short: "Show or change the volume of a Sonos zone",
		Long: `Show or change the volume of a Sonos zone (0-100).

  spot vol Parlor             show current volume
  spot vol Parlor up          increase by 5
  spot vol Parlor up 10       increase by 10
  spot vol Parlor down        decrease by 5
  spot vol Parlor down 10     decrease by 10
  spot vol Parlor 50          set absolute level

After any change, the new volume is printed (clamped to 0-100).`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			zone := args[0]
			if len(args) == 1 {
				v, err := spot.Volume(cmd.Context(), zone)
				if err != nil {
					return err
				}
				return emit(volumeJSON{Zone: zone, Volume: v}, func() error {
					fmt.Println(v)
					return nil
				})
			}
			step := defaultVolumeStep
			if len(args) == 3 {
				n, err := strconv.Atoi(args[2])
				if err != nil || n < 0 {
					return fmt.Errorf("step must be a non-negative integer, got %q", args[2])
				}
				step = n
			}
			var target int
			switch args[1] {
			case "up", "down":
				cur, err := spot.Volume(cmd.Context(), zone)
				if err != nil {
					return err
				}
				if args[1] == "up" {
					target = cur + step
				} else {
					target = cur - step
				}
			default:
				n, err := strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("expected up|down|<level>, got %q", args[1])
				}
				if len(args) == 3 {
					return fmt.Errorf("step argument only valid with up/down")
				}
				target = n
			}
			if target < 0 {
				target = 0
			}
			if target > 100 {
				target = 100
			}
			if err := spot.SetVolume(cmd.Context(), zone, target); err != nil {
				return err
			}
			return emit(volumeJSON{Zone: zone, Volume: target}, func() error {
				fmt.Println(target)
				return nil
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "now <zone>",
		Short: "Show what's playing on a Sonos zone",
		Long: `Print the title, artist, album, and playback state of whatever the
Sonos zone matching <zone> is currently playing. Uses GetTransportInfo +
GetPositionInfo over UPnP/SOAP — works regardless of whether playback was
started via spot, the Sonos app, Spotify Connect, or anything else.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := spot.NowPlaying(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			state := strings.ToLower(strings.ReplaceAll(p.State, "_PLAYBACK", ""))
			out := struct {
				Zone        string `json:"zone"`
				State       string `json:"state"`
				Title       string `json:"title,omitempty"`
				Artist      string `json:"artist,omitempty"`
				Album       string `json:"album,omitempty"`
				Duration    string `json:"duration,omitempty"`
				Position    string `json:"position,omitempty"`
				URI         string `json:"uri,omitempty"`
				AlbumArtURI string `json:"album_art_uri,omitempty"`
			}{
				Zone:        p.Zone,
				State:       state,
				Title:       p.Title,
				Artist:      p.Artist,
				Album:       p.Album,
				Duration:    p.Duration,
				Position:    p.Position,
				URI:         p.URI,
				AlbumArtURI: p.AlbumArtURI,
			}
			return emit(out, func() error {
				fmt.Printf("%s (%s)\n", p.Zone, state)
				if p.Title == "" && p.URI == "" {
					fmt.Println("  nothing loaded")
					return nil
				}
				if p.Artist != "" {
					fmt.Printf("  %s — %s\n", p.Artist, p.Title)
				} else if p.Title != "" {
					fmt.Printf("  %s\n", p.Title)
				}
				if p.Album != "" {
					fmt.Printf("  album: %s\n", p.Album)
				}
				if p.Duration != "" && p.Duration != "0:00:00" {
					fmt.Printf("  %s / %s\n", trimZeroHour(p.Position), trimZeroHour(p.Duration))
				} else if p.Position != "" {
					fmt.Printf("  %s\n", trimZeroHour(p.Position))
				}
				if p.Title == "" && p.URI != "" {
					fmt.Printf("  uri: %s\n", p.URI)
				}
				return nil
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "probe <zone>",
		Short: "Show the URI and DIDL-Lite metadata of what a Sonos zone is playing",
		Long: `Query the Sonos zone for its current TrackURI and TrackMetaData.

Use this to capture the exact format Sonos itself generates: drive Spotify
on the zone from the Sonos app first, then run "spot probe <zone>" to see
what URI and metadata Sonos used. Useful for debugging UPnP error 800 from
"spot play -v sonos" — the URI/metadata format spot generates needs to
match what Sonos expects for your specific Spotify SMAPI integration.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := spot.CurrentTrackFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := struct {
				Zone        string `json:"zone"`
				Coordinator string `json:"coordinator"`
				URI         string `json:"uri"`
				MetadataXML string `json:"metadata_xml"`
			}{Zone: info.Zone, Coordinator: info.IP, URI: info.URI, MetadataXML: info.MetadataXML}
			return emit(out, func() error {
				fmt.Printf("zone:        %s\n", info.Zone)
				fmt.Printf("coordinator: %s\n", info.IP)
				fmt.Printf("\nTrackURI:\n  %s\n", info.URI)
				fmt.Printf("\nTrackMetaData:\n  %s\n", info.MetadataXML)
				return nil
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "diag <zone>",
		Short: "Show how Sonos has Spotify registered for the given zone",
		Long: `Print the Spotify service registration that the Sonos zone reports.
Use this to debug "UPnP error 800" or other playback failures: it shows the
sid (service ID), type (DIDL token number), and sn (account index) that
spot will use when playing on this zone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := spot.SpotifyServiceFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := struct {
				Zone        string `json:"zone"`
				Coordinator string `json:"coordinator"`
				SID         int    `json:"sid"`
				Type        int    `json:"type"`
				SN          int    `json:"sn"`
			}{Zone: info.Zone, Coordinator: info.IP, SID: info.SID, Type: info.Type, SN: info.SN}
			return emit(out, func() error {
				fmt.Printf("zone:        %s\n", info.Zone)
				fmt.Printf("coordinator: %s\n", info.IP)
				fmt.Printf("sid:         %d\n", info.SID)
				fmt.Printf("type:        %d\n", info.Type)
				fmt.Printf("sn:          %d  (override with SONOS_SN env var)\n", info.SN)
				return nil
			})
		},
	})

	// --- personalization commands -------------------------------------
	// `spot top tracks|artists`, `spot recent`, `spot liked`, `spot follows`,
	// `spot playlists`. All read-only. Each uses the matching library
	// function in personalization.go and routes through emit() so --json
	// works. Self-contained block to keep merges with parallel work clean.

	// `spot top` is a parent with two subcommands.
	topCmd := &cobra.Command{
		Use:   "top",
		Short: "Show the current user's top tracks or artists",
		Args:  cobra.NoArgs,
	}

	// parseTopRange maps a positional arg ("", "short", "medium", "long")
	// to the spot.TopRange value the library expects. Default is medium.
	parseTopRange := func(args []string) (spot.TopRange, error) {
		if len(args) == 0 {
			return spot.TopMedium, nil
		}
		switch args[0] {
		case "short":
			return spot.TopShort, nil
		case "medium":
			return spot.TopMedium, nil
		case "long":
			return spot.TopLong, nil
		default:
			return "", fmt.Errorf("invalid time range %q (want short|medium|long)", args[0])
		}
	}

	var topTracksLimit int
	topTracksCmd := &cobra.Command{
		Use:   "tracks [short|medium|long]",
		Short: "List the user's top tracks for a time range (default medium)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseTopRange(args)
			if err != nil {
				return err
			}
			tracks, err := spot.TopTracks(cmd.Context(), r, topTracksLimit)
			if err != nil {
				return err
			}
			out := make([]topTrackJSON, 0, len(tracks))
			for i, t := range tracks {
				artistNames := make([]string, len(t.Artists))
				for j, a := range t.Artists {
					artistNames[j] = a.Name
				}
				out = append(out, topTrackJSON{
					Rank:       i + 1,
					ID:         string(t.ID),
					Title:      t.Name,
					Artist:     strings.Join(artistNames, ", "),
					Popularity: int(t.Popularity),
				})
			}
			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "no top tracks returned")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "RANK\tARTIST\tTITLE\tPOP")
				for _, t := range out {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%d\n", t.Rank, t.Artist, t.Title, t.Popularity)
				}
				return tw.Flush()
			})
		},
	}
	topTracksCmd.Flags().IntVar(&topTracksLimit, "limit", 20, "number of tracks to return (max 50)")
	topCmd.AddCommand(topTracksCmd)

	var topArtistsLimit int
	topArtistsCmd := &cobra.Command{
		Use:   "artists [short|medium|long]",
		Short: "List the user's top artists for a time range (default medium)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseTopRange(args)
			if err != nil {
				return err
			}
			artists, err := spot.TopArtists(cmd.Context(), r, topArtistsLimit)
			if err != nil {
				return err
			}
			out := make([]topArtistJSON, 0, len(artists))
			for i, a := range artists {
				genres := a.Genres
				if genres == nil {
					genres = []string{}
				}
				out = append(out, topArtistJSON{
					Rank:       i + 1,
					ID:         string(a.ID),
					Name:       a.Name,
					Popularity: int(a.Popularity),
					Genres:     genres,
				})
			}
			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "no top artists returned")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "RANK\tNAME\tPOP\tGENRES")
				for _, a := range out {
					fmt.Fprintf(tw, "%d\t%s\t%d\t%s\n", a.Rank, a.Name, a.Popularity, strings.Join(a.Genres, ", "))
				}
				return tw.Flush()
			})
		},
	}
	topArtistsCmd.Flags().IntVar(&topArtistsLimit, "limit", 20, "number of artists to return (max 50)")
	topCmd.AddCommand(topArtistsCmd)

	root.AddCommand(topCmd)

	var recentSince string
	var recentLimit int
	recentCmd := &cobra.Command{
		Use:   "recent",
		Short: "List recently played tracks (last 50, optionally filtered by --since)",
		Long: `List recently-played tracks. Spotify only remembers the last fifty
plays — there is no way to fetch more from the Web API. --since further
filters that list to plays within the given Go duration (e.g. 24h, 168h).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var since time.Duration
			if recentSince != "" {
				d, err := time.ParseDuration(recentSince)
				if err != nil {
					return fmt.Errorf("--since %q: %w", recentSince, err)
				}
				since = d
			}
			items, err := spot.RecentlyPlayed(cmd.Context(), since, recentLimit)
			if err != nil {
				return err
			}
			out := make([]recentTrackJSON, 0, len(items))
			for _, it := range items {
				artistNames := make([]string, len(it.Track.Artists))
				for j, a := range it.Track.Artists {
					artistNames[j] = a.Name
				}
				out = append(out, recentTrackJSON{
					PlayedAt: it.PlayedAt.UTC().Format(time.RFC3339),
					ID:       string(it.Track.ID),
					Title:    it.Track.Name,
					Artist:   strings.Join(artistNames, ", "),
				})
			}
			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "no recently-played tracks")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "WHEN\tARTIST\tTITLE")
				for _, r := range out {
					// human format trims to a compact RFC3339-ish stamp.
					fmt.Fprintf(tw, "%s\t%s\t%s\n", r.PlayedAt, r.Artist, r.Title)
				}
				return tw.Flush()
			})
		},
	}
	recentCmd.Flags().StringVar(&recentSince, "since", "", "filter to plays within this Go duration (e.g. 24h, 168h)")
	recentCmd.Flags().IntVar(&recentLimit, "limit", 50, "max items to fetch from Spotify before --since filtering (max 50)")
	root.AddCommand(recentCmd)

	var likedLimit int
	likedCmd := &cobra.Command{
		Use:   "liked",
		Short: "List the user's saved (liked) tracks, paginated transparently",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tracks, err := spot.LikedTracks(cmd.Context(), likedLimit)
			if err != nil {
				return err
			}
			out := make([]likedTrackJSON, 0, len(tracks))
			for _, t := range tracks {
				artistNames := make([]string, len(t.Artists))
				for j, a := range t.Artists {
					artistNames[j] = a.Name
				}
				out = append(out, likedTrackJSON{
					AddedAt: t.AddedAt,
					ID:      string(t.ID),
					Title:   t.Name,
					Artist:  strings.Join(artistNames, ", "),
				})
			}
			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "no liked tracks")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ADDED\tARTIST\tTITLE")
				for _, t := range out {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", t.AddedAt, t.Artist, t.Title)
				}
				return tw.Flush()
			})
		},
	}
	likedCmd.Flags().IntVar(&likedLimit, "limit", 50, "max tracks to return (paginates as needed)")
	root.AddCommand(likedCmd)

	root.AddCommand(&cobra.Command{
		Use:   "follows",
		Short: "List artists the current user follows (all pages)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			artists, err := spot.FollowedArtists(cmd.Context())
			if err != nil {
				return err
			}
			out := make([]followedArtistJSON, 0, len(artists))
			for _, a := range artists {
				genres := a.Genres
				if genres == nil {
					genres = []string{}
				}
				out = append(out, followedArtistJSON{
					ID:         string(a.ID),
					Name:       a.Name,
					Popularity: int(a.Popularity),
					Genres:     genres,
				})
			}
			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "not following any artists")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tPOP\tGENRES")
				for _, a := range out {
					fmt.Fprintf(tw, "%s\t%d\t%s\n", a.Name, a.Popularity, strings.Join(a.Genres, ", "))
				}
				return tw.Flush()
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "playlists",
		Short: "List playlists owned or followed by the current user (all pages)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pls, err := spot.MyPlaylists(cmd.Context())
			if err != nil {
				return err
			}
			out := make([]playlistJSON, 0, len(pls))
			for _, p := range pls {
				out = append(out, playlistJSON{
					ID:     string(p.ID),
					Name:   p.Name,
					Owner:  p.Owner.DisplayName,
					Tracks: int(p.Tracks.Total),
					Public: p.IsPublic,
				})
			}
			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "no playlists")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tOWNER\tTRACKS\tPUBLIC")
				for _, p := range out {
					fmt.Fprintf(tw, "%s\t%s\t%d\t%t\n", p.Name, p.Owner, p.Tracks, p.Public)
				}
				return tw.Flush()
			})
		},
	})

	var freshDays int
	var freshZone string
	var freshAll bool
	freshCmd := &cobra.Command{
		Use:   "fresh",
		Short: "Recent releases from artists you follow",
		Long: `Walk your followed artists, fetch each one's albums, and list those
released within the last --days (default 30). Pass --zone to queue
the results on a Sonos zone.

By default each fresh album contributes only its first track to the
output and (with --zone) to the queue. Pass --all-tracks to surface
every track on each fresh album.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if freshDays <= 0 {
				return fmt.Errorf("--days must be positive")
			}
			window := time.Duration(freshDays) * 24 * time.Hour
			albums, err := spot.Fresh(cmd.Context(), window, 0)
			if err != nil {
				return err
			}

			// Optionally expand each album to its full track list.
			if freshAll {
				for i := range albums {
					ids, err := spot.AlbumTrackIDs(cmd.Context(), albums[i].AlbumID)
					if err == nil && len(ids) > 0 {
						albums[i].TrackIDs = ids
					}
				}
			}

			out := make([]freshAlbumJSON, 0, len(albums))
			for _, a := range albums {
				ids := a.TrackIDs
				if ids == nil {
					ids = []string{}
				}
				out = append(out, freshAlbumJSON{
					AlbumID:     a.AlbumID,
					AlbumName:   a.AlbumName,
					ArtistID:    a.ArtistID,
					ArtistName:  a.ArtistName,
					ReleaseDate: a.ReleaseDate.Format("2006-01-02"),
					TrackIDs:    ids,
				})
			}

			// If a zone was supplied, gather every TrackID in newest-first
			// order and queue them on the zone before printing.
			if freshZone != "" && len(albums) > 0 {
				var queue []string
				for _, a := range albums {
					queue = append(queue, a.TrackIDs...)
				}
				if len(queue) > 0 {
					if err := spot.PlayTrackIDsViaSonos(cmd.Context(), freshZone, queue); err != nil {
						return err
					}
				}
			}

			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintf(os.Stderr, "no fresh releases from followed artists in the last %d days\n", freshDays)
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "RELEASED\tARTIST\tALBUM")
				for _, a := range out {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", a.ReleaseDate, a.ArtistName, a.AlbumName)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				if freshZone != "" {
					fmt.Printf("queued %d album(s) on %s\n", len(out), freshZone)
				}
				return nil
			})
		},
	}
	freshCmd.Flags().IntVar(&freshDays, "days", 30, "lookback window in days")
	freshCmd.Flags().StringVar(&freshZone, "zone", "", "queue the result on this Sonos zone (omit to just print)")
	freshCmd.Flags().BoolVar(&freshAll, "all-tracks", false, "include all tracks per album (default: first track only)")
	root.AddCommand(freshCmd)

	// --- mix command (bd-bqx) -----------------------------------------

	var (
		mixLength        int
		mixSeed          string
		mixZone          string
		mixExcludeRecent int
	)
	mixCmd := &cobra.Command{
		Use:   "mix",
		Short: "Curated mix from your top tracks (or a seed) with feedback/recent filters",
		Long: `Build a curated track list from your medium_term top tracks (or, with
--seed, the seed track plus its primary artist's top tracks), filtered
against the local feedback memory (skip-forever / hate verdicts) and
recent kind='play_started' history. Truncated to --length.

Pass --zone to queue the resulting mix on a Sonos zone via UPnP after
emitting it; omit --zone to just print the list.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mixLength <= 0 {
				return fmt.Errorf("--length must be positive")
			}
			if mixExcludeRecent < 0 {
				return fmt.Errorf("--exclude-recent must be >= 0")
			}
			opts := spot.MixOptions{
				Length:        mixLength,
				Seed:          mixSeed,
				ExcludeRecent: time.Duration(mixExcludeRecent) * 24 * time.Hour,
			}
			tracks, err := spot.Mix(cmd.Context(), opts)
			if err != nil {
				return err
			}

			out := mixResultJSON{
				Length: len(tracks),
				Seed:   mixSeed,
				Zone:   mixZone,
				Tracks: make([]mixTrackJSON, 0, len(tracks)),
			}
			ids := make([]string, 0, len(tracks))
			for i, t := range tracks {
				out.Tracks = append(out.Tracks, mixTrackJSON{
					Position: i + 1,
					TrackID:  t.TrackID,
					Title:    t.Title,
					Artist:   t.Artist,
					Album:    t.Album,
				})
				if t.TrackID != "" {
					ids = append(ids, t.TrackID)
				}
			}

			if mixZone != "" && len(ids) > 0 {
				if err := spot.PlayTrackIDsViaSonos(cmd.Context(), mixZone, ids); err != nil {
					return err
				}
			}

			return emit(out, func() error {
				if len(tracks) == 0 {
					fmt.Fprintln(os.Stderr, "no tracks in mix (try a different seed or shorter --exclude-recent)")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "POS\tARTIST\tTITLE")
				for i, t := range tracks {
					fmt.Fprintf(tw, "%d\t%s\t%s\n", i+1, t.Artist, t.Title)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				if mixZone != "" {
					fmt.Printf("queued %d track(s) on %s\n", len(ids), mixZone)
				}
				return nil
			})
		},
	}
	mixCmd.Flags().IntVar(&mixLength, "length", 20, "number of tracks in the mix")
	mixCmd.Flags().StringVar(&mixSeed, "seed", "", "anchor mix to this track or artist (URI or query)")
	mixCmd.Flags().StringVar(&mixZone, "zone", "", "queue the result on this Sonos zone (omit to just print)")
	mixCmd.Flags().IntVar(&mixExcludeRecent, "exclude-recent", 7, "drop tracks played within the last N days (0 disables)")
	root.AddCommand(mixCmd)

	// --- daily command (bd-wiz) ---------------------------------------

	var dailyZone string
	var dailyLength int
	dailyCmd := &cobra.Command{
		Use:   "daily",
		Short: "Top long-term tracks minus what you've heard recently",
		Long: `Spotify-Daily replacement: surface long-term top tracks the user
has not played in the last 30 days, with local feedback applied.

Tracks marked "hate" or "skip-forever" via "spot feedback" are dropped.
Tracks marked "love" are boosted to the front of the list. The remainder
keeps its long-term-rank order.

Pass --zone to queue the result on a Sonos zone via PlayTrackIDsViaSonos;
omit to just print (or emit JSON with --json).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tracks, err := spot.Daily(cmd.Context(), spot.DailyOptions{Length: dailyLength})
			if err != nil {
				return err
			}
			out := make([]dailyTrackJSON, 0, len(tracks))
			for i, t := range tracks {
				out = append(out, dailyTrackJSON{
					Rank:    i + 1,
					TrackID: t.TrackID,
					Title:   t.Title,
					Artist:  t.Artist,
					Album:   t.Album,
				})
			}

			if dailyZone != "" && len(tracks) > 0 {
				ids := make([]string, 0, len(tracks))
				for _, t := range tracks {
					if t.TrackID != "" {
						ids = append(ids, t.TrackID)
					}
				}
				if len(ids) > 0 {
					if err := spot.PlayTrackIDsViaSonos(cmd.Context(), dailyZone, ids); err != nil {
						return err
					}
				}
			}

			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "no daily tracks (need top long-term tracks; try Spotify for a few weeks)")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "RANK\tARTIST\tTITLE")
				for _, t := range out {
					fmt.Fprintf(tw, "%d\t%s\t%s\n", t.Rank, t.Artist, t.Title)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				if dailyZone != "" {
					fmt.Printf("queued %d track(s) on %s\n", len(out), dailyZone)
				}
				return nil
			})
		},
	}
	dailyCmd.Flags().StringVar(&dailyZone, "zone", "", "queue the result on this Sonos zone (omit to just print)")
	dailyCmd.Flags().IntVar(&dailyLength, "length", 20, "max number of tracks to return")
	root.AddCommand(dailyCmd)

	// --- deeper command (bd-1ky) --------------------------------------

	var deeperZone string
	var deeperPerAlbum int
	deeperCmd := &cobra.Command{
		Use:   "deeper <artist>",
		Short: "Surface deep cuts: an artist's albums minus their top 10 tracks",
		Long: `Walk an artist's full-length albums in chronological order (oldest
first) and pick 1-2 tracks from each — skipping the artist's top-10
"greatest hits" so you get the deeper material instead. Singles,
compilations, and appears-on records are filtered out.

<artist> may be a free-text query ("radiohead") or a Spotify artist URI
("spotify:artist:..."). With --zone, the resulting track list is queued
on the named Sonos zone via UPnP.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tracks, err := spot.Deeper(cmd.Context(), args[0], spot.DeeperOptions{PerAlbum: deeperPerAlbum})
			if err != nil {
				return err
			}
			out := make([]deeperTrackJSON, 0, len(tracks))
			for _, t := range tracks {
				out = append(out, deeperTrackJSON{
					AlbumID:     t.AlbumID,
					AlbumName:   t.AlbumName,
					ReleaseDate: t.ReleaseDate.Format("2006-01-02"),
					TrackID:     t.TrackID,
					Title:       t.Title,
					Artist:      t.Artist,
				})
			}
			if deeperZone != "" && len(tracks) > 0 {
				ids := make([]string, 0, len(tracks))
				for _, t := range tracks {
					ids = append(ids, t.TrackID)
				}
				if err := spot.PlayTrackIDsViaSonos(cmd.Context(), deeperZone, ids); err != nil {
					return err
				}
			}
			return emit(out, func() error {
				if len(out) == 0 {
					fmt.Fprintln(os.Stderr, "no deeper tracks found")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "RELEASED\tALBUM\tTITLE")
				for _, t := range out {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", t.ReleaseDate, t.AlbumName, t.Title)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				if deeperZone != "" {
					fmt.Printf("queued %d track(s) on %s\n", len(out), deeperZone)
				}
				return nil
			})
		},
	}
	deeperCmd.Flags().StringVar(&deeperZone, "zone", "", "queue the result on this Sonos zone (omit to just print)")
	deeperCmd.Flags().IntVar(&deeperPerAlbum, "per-album", 1, "tracks to surface per album (1 or 2 keeps the queue tight)")
	root.AddCommand(deeperCmd)

	// --- rabbithole command (bd-1aw) ----------------------------------

	var (
		rabbitSeed   string
		rabbitLength int
		rabbitZone   string
	)
	rabbitCmd := &cobra.Command{
		Use:   "rabbithole",
		Short: "Walk from a seed artist through genre overlap with your top artists",
		Long: `Substitute for Spotify's deprecated related-artists endpoint. Resolve a
seed artist (--seed URI/query, or default: your top short-term artist),
read its genre tags, score your medium- and long-term top artists by how
many of those genres they share, take the top eight, and pull a couple of
top tracks from each. Returns roughly --length tracks (default 20).

Pass --zone to queue the result on a Sonos zone instead of just printing.

The JSON output also includes the artist-by-artist walk that produced
the track list, so a future --explain flag can show why each artist
was picked.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tracks, walk, err := spot.Rabbithole(cmd.Context(), spot.RabbitholeOptions{
				Seed:   rabbitSeed,
				Length: rabbitLength,
			})
			if err != nil {
				return err
			}

			if rabbitZone != "" && len(tracks) > 0 {
				ids := make([]string, 0, len(tracks))
				for _, t := range tracks {
					ids = append(ids, t.TrackID)
				}
				if err := spot.PlayTrackIDsViaSonos(cmd.Context(), rabbitZone, ids); err != nil {
					return err
				}
			}

			outTracks := make([]rabbitholeTrackJSON, 0, len(tracks))
			for _, t := range tracks {
				outTracks = append(outTracks, rabbitholeTrackJSON{
					TrackID: t.TrackID,
					Title:   t.Title,
					Artist:  t.Artist,
					Album:   t.Album,
				})
			}
			outWalk := make([]rabbitholeStepJSON, 0, len(walk))
			for _, s := range walk {
				ids := s.TrackIDs
				if ids == nil {
					ids = []string{}
				}
				outWalk = append(outWalk, rabbitholeStepJSON{
					ArtistID:     s.ArtistID,
					ArtistName:   s.ArtistName,
					GenreOverlap: s.GenreOverlap,
					TrackIDs:     ids,
				})
			}
			env := rabbitholeJSON{
				Seed:   rabbitSeed,
				Zone:   rabbitZone,
				Tracks: outTracks,
				Walk:   outWalk,
			}
			return emit(env, func() error {
				if len(tracks) == 0 {
					fmt.Fprintln(os.Stderr, "rabbithole: no tracks (try a --seed with at least one genre that overlaps your top artists)")
					return nil
				}
				for _, t := range tracks {
					if t.Artist != "" {
						fmt.Printf("%s — %s\n", t.Artist, t.Title)
					} else {
						fmt.Println(t.Title)
					}
				}
				if rabbitZone != "" {
					fmt.Printf("queued %d track(s) on %s\n", len(tracks), rabbitZone)
				}
				return nil
			})
		},
	}
	rabbitCmd.Flags().StringVar(&rabbitSeed, "seed", "", "seed artist (free-text query or spotify:artist:<id>); default: your top short-term artist")
	rabbitCmd.Flags().IntVar(&rabbitLength, "length", 20, "max number of tracks to return")
	rabbitCmd.Flags().StringVar(&rabbitZone, "zone", "", "queue the result on this Sonos zone (omit to just print)")
	root.AddCommand(rabbitCmd)

	// --- end personalization commands ---------------------------------

	var via string
	var continuePlay bool
	playCmd := &cobra.Command{
		Use:   `play <query> <room>`,
		Short: "Search for a track and play it on a named device or zone",
		Long: `Search for a track and play it on a device whose name contains <room>
(case-insensitive substring match).

<query> may be free-text ("bonobo migration") or a Spotify URI
("spotify:track:4cOdK2wGLETKBW3PvgPWqT"). The top search result is used.

Transport (-v / --via):
  connect (default) — Spotify Web API targets a Spotify Connect device.
                      Works for Echo, modern Sonos with Connect support, etc.
  sonos             — UPnP/SOAP directly to the Sonos coordinator. Works on
                      every Sonos speaker that has Spotify linked, including
                      older hardware that doesn't register as a Connect
                      endpoint (Play:1, Play:3, One SL on old firmware).

Continuation (-c / --continue):
  After the seed track, queue the primary artist's top tracks (~10 more).
  In Connect mode they're passed as a URI list to start_playback. In Sonos
  mode they're added to the speaker's queue and the queue is played.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var opts []spot.PlayOption
			if continuePlay {
				opts = append(opts, spot.WithContinue())
			}
			switch via {
			case "", "connect":
				if err := spot.Play(cmd.Context(), args[0], args[1], opts...); err != nil {
					return err
				}
			case "sonos":
				if err := spot.PlayViaSonos(cmd.Context(), args[0], args[1], opts...); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown --via=%q (expected 'connect' or 'sonos')", via)
			}
			return emit(okResult{OK: true, Command: "play", Zone: args[1]}, nil)
		},
	}
	playCmd.Flags().StringVarP(&via, "via", "v", "connect", "playback transport: connect | sonos")
	playCmd.Flags().BoolVarP(&continuePlay, "continue", "c", false, "after the seed track, queue more from the primary artist")
	root.AddCommand(playCmd)

	root.AddCommand(newFeedbackCmd())
	root.AddCommand(newMarkCmd())
	root.AddCommand(newHistoryCmd())
	root.AddCommand(newStatsCmd())

	// resolveLibraryQuery picks between an explicit query arg and --current
	// <zone> mode. With --current, NowPlaying drives the URI.
	resolveLibraryQuery := func(cmd *cobra.Command, args []string, currentZone string) (string, error) {
		if currentZone != "" {
			if len(args) > 0 {
				return "", fmt.Errorf("pass either <query> or --current <zone>, not both")
			}
			p, err := spot.NowPlaying(cmd.Context(), currentZone)
			if err != nil {
				return "", err
			}
			if p.URI == "" {
				return "", fmt.Errorf("nothing playing on %q", currentZone)
			}
			return p.URI, nil
		}
		if len(args) != 1 {
			return "", fmt.Errorf("requires <query> or --current <zone>")
		}
		return args[0], nil
	}

	var likeCurrent string
	likeCmd := &cobra.Command{
		Use:   "like [<query>|--current <zone>]",
		Short: "Save a track to your Liked Songs library",
		Long: `Resolve <query> (free-text or "spotify:track:..." URI) to a single
track and add it to your Liked Songs (PUT /v1/me/tracks).

With --current <zone>, the currently playing track on the named Sonos zone
is liked instead — handy for "like what I'm hearing right now".`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := resolveLibraryQuery(cmd, args, likeCurrent)
			if err != nil {
				return err
			}
			info, err := spot.Like(cmd.Context(), q)
			if err != nil {
				return err
			}
			return emit(libTrackJSON{
				Action: "like", TrackID: info.TrackID, Title: info.Title, Artist: info.Artist,
			}, func() error {
				fmt.Printf("liked: %s — %s\n", info.Artist, info.Title)
				return nil
			})
		},
	}
	likeCmd.Flags().StringVar(&likeCurrent, "current", "", "like the track currently playing on the named Sonos zone")
	root.AddCommand(likeCmd)

	var unlikeCurrent string
	unlikeCmd := &cobra.Command{
		Use:   "unlike [<query>|--current <zone>]",
		Short: "Remove a track from your Liked Songs library",
		Long: `Resolve <query> (free-text or "spotify:track:..." URI) to a single
track and remove it from your Liked Songs (DELETE /v1/me/tracks).

With --current <zone>, the currently playing track on the named Sonos zone
is unliked instead.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := resolveLibraryQuery(cmd, args, unlikeCurrent)
			if err != nil {
				return err
			}
			info, err := spot.Unlike(cmd.Context(), q)
			if err != nil {
				return err
			}
			return emit(libTrackJSON{
				Action: "unlike", TrackID: info.TrackID, Title: info.Title, Artist: info.Artist,
			}, func() error {
				fmt.Printf("unliked: %s — %s\n", info.Artist, info.Title)
				return nil
			})
		},
	}
	unlikeCmd.Flags().StringVar(&unlikeCurrent, "current", "", "unlike the track currently playing on the named Sonos zone")
	root.AddCommand(unlikeCmd)

	root.AddCommand(&cobra.Command{
		Use:   "follow <artist>",
		Short: "Follow an artist",
		Long: `Resolve <artist> (free-text query or "spotify:artist:..." URI) to a
single artist via Search and follow them
(PUT /v1/me/following?type=artist).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, name, err := spot.Follow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(followJSON{Action: "follow", ArtistID: id, Name: name}, func() error {
				fmt.Printf("following: %s\n", name)
				return nil
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "unfollow <artist>",
		Short: "Unfollow an artist",
		Long: `Resolve <artist> (free-text query or "spotify:artist:..." URI) to a
single artist via Search and unfollow them
(DELETE /v1/me/following?type=artist).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, name, err := spot.Unfollow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(followJSON{Action: "unfollow", ArtistID: id, Name: name}, func() error {
				fmt.Printf("unfollowed: %s\n", name)
				return nil
			})
		},
	})

	playlistCmd := &cobra.Command{
		Use:   "playlist",
		Short: "Manage your Spotify playlists (new, add, remove)",
	}

	var playlistNewDescription string
	var playlistNewPublic bool
	playlistNewCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new playlist owned by the current user",
		Long: `Create a new playlist owned by the authenticated user
(POST /v1/users/{me.id}/playlists). Playlists are private by default;
pass --public to create a public playlist instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pl, err := spot.CreatePlaylist(cmd.Context(), args[0], playlistNewDescription, playlistNewPublic)
			if err != nil {
				return err
			}
			return emit(playlistNewJSON{
				ID: string(pl.ID), Name: pl.Name, Public: pl.IsPublic, URI: string(pl.URI),
			}, func() error {
				fmt.Printf("created playlist: %s\n  id:  %s\n  uri: %s\n", pl.Name, pl.ID, pl.URI)
				return nil
			})
		},
	}
	playlistNewCmd.Flags().StringVar(&playlistNewDescription, "description", "", "playlist description")
	playlistNewCmd.Flags().BoolVar(&playlistNewPublic, "public", false, "make the playlist public (default: private)")
	playlistCmd.AddCommand(playlistNewCmd)

	playlistCmd.AddCommand(&cobra.Command{
		Use:   "add <playlist> <query>",
		Short: "Add a track to one of your playlists",
		Long: `Match <playlist> by case-insensitive substring against your playlists
(MyPlaylists), resolve <query> to a track, and add it
(POST /v1/playlists/{id}/tracks). Errors if zero or more than one playlist
match.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, pl, err := spot.AddToPlaylist(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return emit(playlistMutateJSON{
				Action:   "playlist-add",
				Playlist: pl.Name,
				Track:    playlistTrackJSON{TrackID: info.TrackID, Title: info.Title, Artist: info.Artist},
			}, func() error {
				fmt.Printf("added to %s: %s — %s\n", pl.Name, info.Artist, info.Title)
				return nil
			})
		},
	})

	playlistCmd.AddCommand(&cobra.Command{
		Use:   "remove <playlist> <query>",
		Short: "Remove a track from one of your playlists",
		Long: `Match <playlist> by case-insensitive substring against your playlists
(MyPlaylists), resolve <query> to a track, and remove it
(DELETE /v1/playlists/{id}/tracks). Errors if zero or more than one playlist
match.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, pl, err := spot.RemoveFromPlaylist(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return emit(playlistMutateJSON{
				Action:   "playlist-remove",
				Playlist: pl.Name,
				Track:    playlistTrackJSON{TrackID: info.TrackID, Title: info.Title, Artist: info.Artist},
			}, func() error {
				fmt.Printf("removed from %s: %s — %s\n", pl.Name, info.Artist, info.Title)
				return nil
			})
		},
	})

	root.AddCommand(playlistCmd)

	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

// feedbackJSON is the JSON shape for `spot feedback`.
type feedbackJSON struct {
	Action  string `json:"action"`
	Verdict string `json:"verdict"`
	TrackID string `json:"track_id"`
}

// markJSON is the JSON shape for `spot mark`.
type markJSON struct {
	Action  string `json:"action"`
	Note    string `json:"note"`
	Zone    string `json:"zone,omitempty"`
	TrackID string `json:"track_id"`
}

// historyTrackJSON is the cached-track sub-object on a history row.
type historyTrackJSON struct {
	TrackID  string `json:"track_id"`
	Title    string `json:"title,omitempty"`
	Artist   string `json:"artist,omitempty"`
	ArtistID string `json:"artist_id,omitempty"`
	Album    string `json:"album,omitempty"`
}

// historyRowJSON is one event in `spot history --json`.
type historyRowJSON struct {
	ID         int64             `json:"id"`
	Kind       string            `json:"kind"`
	Zone       string            `json:"zone,omitempty"`
	Source     string            `json:"source,omitempty"`
	OccurredAt string            `json:"occurred_at"`
	Track      *historyTrackJSON `json:"track,omitempty"`
}

func newFeedbackCmd() *cobra.Command {
	var current bool
	var zone, note string
	cmd := &cobra.Command{
		Use:   "feedback <love|hate|skip-forever> [query]",
		Short: "Record a verdict on a track for the local-memory layer",
		Long: `Record a feedback event ("love", "hate", or "skip-forever") on a track.
Either pass a free-text query / Spotify URI, or use --current with a zone to
target whatever's currently playing on that zone.

The event is written to the local SQLite memory and the resolved track is
also cached so future "spot history" rows can show its title and artist.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			verdict := args[0]
			var query string
			if len(args) == 2 {
				query = args[1]
			}
			if !current && query == "" {
				return fmt.Errorf("either pass a query or --current <zone>")
			}
			if current && zone == "" {
				return fmt.Errorf("--current requires --zone <name>")
			}
			info, err := spot.Feedback(cmd.Context(), query, zone, verdict, note)
			if err != nil {
				return err
			}
			out := feedbackJSON{Action: "feedback", Verdict: verdict, TrackID: info.TrackID}
			return emit(out, func() error {
				label := info.Title
				if info.Artist != "" {
					label = info.Artist + " — " + info.Title
				}
				fmt.Printf("%s recorded for %s (%s)\n", verdict, label, info.TrackID)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&current, "current", false, "feedback applies to the zone's currently-playing track")
	cmd.Flags().StringVar(&zone, "zone", "", "Sonos zone name (required with --current)")
	cmd.Flags().StringVar(&note, "note", "", "optional free-text note attached to the feedback event")
	return cmd
}

func newMarkCmd() *cobra.Command {
	var zone string
	cmd := &cobra.Command{
		Use:   "mark <note>",
		Short: "Annotate the currently-playing track with a free-text note",
		Long: `Record a "note" event for whatever is currently playing on a zone. With
no --zone, mark picks the first zone in the household whose state is
PLAYING; if none are playing, mark errors out and asks for an explicit zone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			note := args[0]
			info, err := spot.Mark(cmd.Context(), zone, note)
			if err != nil {
				return err
			}
			out := markJSON{Action: "mark", Note: note, Zone: zone, TrackID: info.TrackID}
			return emit(out, func() error {
				label := info.Title
				if info.Artist != "" {
					label = info.Artist + " — " + info.Title
				}
				fmt.Printf("noted on %s (%s): %s\n", label, info.TrackID, note)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Sonos zone (default: first zone reporting PLAYING)")
	return cmd
}

func newHistoryCmd() *cobra.Command {
	var (
		zone, artist, kind, since string
		limit                     int
	)
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show recent events from the local-memory layer",
		Long: `Print rows from the events table joined to the cached track metadata.

Filters:
  --zone NAME       only events from this zone
  --artist X        substring match against cached artist name, or
                    "spotify:artist:<id>" for an exact id match
  --kind X          only events of this kind (observe, play, feedback, note, ...)
  --since DURATION  Go duration relative to now (default 24h)
  --limit N         max rows (default 50)

Newest first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := spot.HistoryFilter{Zone: zone, Artist: artist, Kind: kind, Limit: limit}
			if since != "" {
				d, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("--since: %w", err)
				}
				f.Since = d
			}
			rows, err := spot.History(cmd.Context(), f)
			if err != nil {
				return err
			}
			out := make([]historyRowJSON, 0, len(rows))
			for _, r := range rows {
				row := historyRowJSON{
					ID:         r.ID,
					Kind:       r.Kind,
					Zone:       r.Zone,
					Source:     r.Source,
					OccurredAt: r.OccurredAt.UTC().Format(time.RFC3339),
				}
				if r.Track != nil {
					row.Track = &historyTrackJSON{
						TrackID:  r.Track.TrackID,
						Title:    r.Track.Title,
						Artist:   r.Track.Artist,
						ArtistID: r.Track.ArtistID,
						Album:    r.Track.Album,
					}
				}
				out = append(out, row)
			}
			return emit(out, func() error {
				if len(rows) == 0 {
					fmt.Fprintln(os.Stderr, "no events match")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "WHEN\tKIND\tZONE\tTRACK")
				for _, r := range rows {
					ts := r.OccurredAt.Local().Format("2006-01-02 15:04")
					label := ""
					if r.Track != nil {
						if r.Track.Artist != "" && r.Track.Title != "" {
							label = r.Track.Artist + " — " + r.Track.Title
						} else if r.Track.Title != "" {
							label = r.Track.Title
						} else {
							label = r.Track.TrackID
						}
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ts, r.Kind, r.Zone, label)
				}
				return tw.Flush()
			})
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "filter by Sonos zone name")
	cmd.Flags().StringVar(&artist, "artist", "", "filter by artist (substring or spotify:artist:<id>)")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by event kind")
	cmd.Flags().StringVar(&since, "since", "24h", "Go duration window relative to now")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to return")
	return cmd
}

func newStatsCmd() *cobra.Command {
	var (
		topN    int
		recent  bool
		skipped bool
	)
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show aggregations from the local-memory layer",
		Long: `Print play / skip aggregations sourced from the events table.

Modes (mutually exclusive — default is --top 10):
  --top N    most-played tracks and artists from kind='play_started' (all time)
  --recent   same as --top but limited to the last 7 days
  --skipped  tracks with the most kind='skip' events in the last 7 days`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			modes := 0
			if cmd.Flags().Changed("top") {
				modes++
			}
			if recent {
				modes++
			}
			if skipped {
				modes++
			}
			if modes > 1 {
				return fmt.Errorf("--top, --recent, --skipped are mutually exclusive")
			}
			mode := "top"
			n := topN
			switch {
			case recent:
				mode = "recent"
			case skipped:
				mode = "skipped"
			}
			s, err := spot.ComputeStats(cmd.Context(), mode, n)
			if err != nil {
				return err
			}
			return emit(s, func() error {
				switch mode {
				case "skipped":
					if len(s.Skipped) == 0 {
						fmt.Println("no skip events in the last 7 days")
						return nil
					}
					tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
					fmt.Fprintln(tw, "COUNT\tTRACK")
					for _, t := range s.Skipped {
						label := t.TrackID
						if t.Artist != "" && t.Title != "" {
							label = t.Artist + " — " + t.Title
						}
						fmt.Fprintf(tw, "%d\t%s\n", t.Count, label)
					}
					return tw.Flush()
				default:
					if len(s.TopTracks) == 0 && len(s.TopArtists) == 0 {
						fmt.Println("no play_started events recorded yet")
						return nil
					}
					tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
					fmt.Fprintln(tw, "TOP TRACKS\t\tTOP ARTISTS")
					rows := len(s.TopTracks)
					if len(s.TopArtists) > rows {
						rows = len(s.TopArtists)
					}
					for i := 0; i < rows; i++ {
						left := ""
						if i < len(s.TopTracks) {
							t := s.TopTracks[i]
							label := t.TrackID
							if t.Artist != "" && t.Title != "" {
								label = t.Artist + " — " + t.Title
							}
							left = fmt.Sprintf("%d  %s", t.Count, label)
						}
						right := ""
						if i < len(s.TopArtists) {
							a := s.TopArtists[i]
							name := a.Name
							if name == "" {
								name = a.ArtistID
							}
							right = fmt.Sprintf("%d  %s", a.Count, name)
						}
						fmt.Fprintf(tw, "%s\t\t%s\n", left, right)
					}
					return tw.Flush()
				}
			})
		},
	}
	cmd.Flags().IntVar(&topN, "top", 10, "limit for top-N aggregations")
	cmd.Flags().BoolVar(&recent, "recent", false, "restrict --top window to the last 7 days")
	cmd.Flags().BoolVar(&skipped, "skipped", false, "show most-skipped tracks in the last 7 days")
	return cmd
}
