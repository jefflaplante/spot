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

	"github.com/jefflaplante/spotify"
	"github.com/spf13/cobra"
)

const defaultVolumeStep = 5

// trimZeroHour drops a leading "0:" from Sonos's hh:mm:ss timestamps so
// "0:03:13" displays as "03:13".
func trimZeroHour(t string) string {
	return strings.TrimPrefix(t, "0:")
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

	root.AddCommand(&cobra.Command{
		Use:   "auth",
		Short: "Run the one-time OAuth bootstrap and write the token cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return spotify.Authenticate(cmd.Context())
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
			devs, err := spotify.Devices(cmd.Context())
			if err != nil {
				return err
			}
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
			zones, err := spotify.ListSonosZones(cmd.Context())
			if err != nil {
				return err
			}
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
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "pause <zone>",
		Short: "Pause playback on a Sonos zone",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return spotify.Pause(cmd.Context(), args[0])
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume <zone>",
		Short: "Resume playback on a Sonos zone (continues from current position)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return spotify.Resume(cmd.Context(), args[0])
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
			res, err := spotify.EnqueueViaSonos(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("%s: queued at position %d (queue now has %d tracks)\n",
				res.Zone, res.Position, res.NewLength)
			return nil
		},
	})

	var queueRaw bool
	queueCmd := &cobra.Command{
		Use:   "queue <zone>",
		Short: "Show the playback queue and current position",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if queueRaw {
				xmlStr, err := spotify.BrowseQueueRaw(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				fmt.Println(xmlStr)
				return nil
			}
			q, err := spotify.ShowQueue(cmd.Context(), args[0])
			if err != nil {
				return err
			}
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
			return spotify.Next(cmd.Context(), args[0])
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "stop <zone>",
		Short: "Stop playback on a Sonos zone (resets position to start)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return spotify.Stop(cmd.Context(), args[0])
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
				v, err := spotify.Volume(cmd.Context(), zone)
				if err != nil {
					return err
				}
				fmt.Println(v)
				return nil
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
				cur, err := spotify.Volume(cmd.Context(), zone)
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
			if err := spotify.SetVolume(cmd.Context(), zone, target); err != nil {
				return err
			}
			fmt.Println(target)
			return nil
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
			p, err := spotify.NowPlaying(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			state := strings.ToLower(strings.ReplaceAll(p.State, "_PLAYBACK", ""))
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
			info, err := spotify.CurrentTrackFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("zone:        %s\n", info.Zone)
			fmt.Printf("coordinator: %s\n", info.IP)
			fmt.Printf("\nTrackURI:\n  %s\n", info.URI)
			fmt.Printf("\nTrackMetaData:\n  %s\n", info.MetadataXML)
			return nil
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
			info, err := spotify.SpotifyServiceFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("zone:        %s\n", info.Zone)
			fmt.Printf("coordinator: %s\n", info.IP)
			fmt.Printf("sid:         %d\n", info.SID)
			fmt.Printf("type:        %d\n", info.Type)
			fmt.Printf("sn:          %d  (override with SONOS_SN env var)\n", info.SN)
			return nil
		},
	})

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
			var opts []spotify.PlayOption
			if continuePlay {
				opts = append(opts, spotify.WithContinue())
			}
			switch via {
			case "", "connect":
				return spotify.Play(cmd.Context(), args[0], args[1], opts...)
			case "sonos":
				return spotify.PlayViaSonos(cmd.Context(), args[0], args[1], opts...)
			default:
				return fmt.Errorf("unknown --via=%q (expected 'connect' or 'sonos')", via)
			}
		},
	}
	playCmd.Flags().StringVarP(&via, "via", "v", "connect", "playback transport: connect | sonos")
	playCmd.Flags().BoolVarP(&continuePlay, "continue", "c", false, "after the seed track, queue more from the primary artist")
	root.AddCommand(playCmd)

	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
