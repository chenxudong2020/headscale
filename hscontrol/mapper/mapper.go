package mapper

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/juanfont/headscale/hscontrol/db"
	"github.com/juanfont/headscale/hscontrol/policy"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/juanfont/headscale/hscontrol/util"
	"github.com/klauspost/compress/zstd"
	"github.com/rs/zerolog/log"
	"tailscale.com/smallzstd"
	"tailscale.com/tailcfg"
	"tailscale.com/types/dnstype"
	"tailscale.com/types/key"
)

const (
	nextDNSDoHPrefix           = "https://dns.nextdns.io"
	reservedResponseHeaderSize = 4
)

type Mapper struct {
	db *db.HSDatabase

	privateKey2019 *key.MachinePrivate
	isNoise        bool

	// Configuration
	// TODO(kradalby): figure out if this is the format we want this in
	derpMap          *tailcfg.DERPMap
	baseDomain       string
	dnsCfg           *tailcfg.DNSConfig
	logtail          bool
	randomClientPort bool
	stripEmailDomain bool
}

func NewMapper(
	db *db.HSDatabase,
	privateKey *key.MachinePrivate,
	isNoise bool,
	derpMap *tailcfg.DERPMap,
	baseDomain string,
	dnsCfg *tailcfg.DNSConfig,
	logtail bool,
	randomClientPort bool,
	stripEmailDomain bool,
) *Mapper {
	return &Mapper{
		db: db,

		privateKey2019: privateKey,
		isNoise:        isNoise,

		derpMap:          derpMap,
		baseDomain:       baseDomain,
		dnsCfg:           dnsCfg,
		logtail:          logtail,
		randomClientPort: randomClientPort,
		stripEmailDomain: stripEmailDomain,
	}
}

func (m Mapper) fullMapResponse(
	mapRequest tailcfg.MapRequest,
	machine *types.Machine,
	pol *policy.ACLPolicy,
) (*tailcfg.MapResponse, error) {
	log.Trace().
		Caller().
		Str("machine", mapRequest.Hostinfo.Hostname).
		Msg("Creating Map response")

	// TODO(kradalby): Decouple this from DB?
	node, err := m.db.TailNode(*machine, pol, m.dnsCfg)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot convert to node")

		return nil, err
	}

	peers, err := m.db.ListPeers(machine)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot fetch peers")

		return nil, err
	}

	rules, sshPolicy, err := policy.GenerateFilterRules(pol, peers, m.stripEmailDomain)
	if err != nil {
		return nil, err
	}

	if len(rules) > 0 {
		peers = policy.FilterMachinesByACL(machine, peers, rules)
	}

	profiles := generateUserProfiles(machine, peers, m.baseDomain)

	// TODO(kradalby): Decouple this from DB?
	nodePeers, err := m.db.TailNodes(peers, pol, m.dnsCfg)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Failed to convert peers to Tailscale nodes")

		return nil, err
	}

	// TODO(kradalby): Shold this mutation happen before TailNode(s) is called?
	dnsConfig := generateDNSConfig(
		m.dnsCfg,
		m.baseDomain,
		*machine,
		peers,
	)

	now := time.Now()

	resp := tailcfg.MapResponse{
		KeepAlive: false,
		Node:      node,

		// TODO: Only send if updated
		DERPMap: m.derpMap,

		// TODO: Only send if updated
		Peers: nodePeers,

		// TODO(kradalby): Implement:
		// https://github.com/tailscale/tailscale/blob/main/tailcfg/tailcfg.go#L1351-L1374
		// PeersChanged
		// PeersRemoved
		// PeersChangedPatch
		// PeerSeenChange
		// OnlineChange

		// TODO: Only send if updated
		DNSConfig: dnsConfig,

		// TODO: Only send if updated
		Domain: m.baseDomain,

		// Do not instruct clients to collect services, we do not
		// support or do anything with them
		CollectServices: "false",

		// TODO: Only send if updated
		PacketFilter: rules,

		UserProfiles: profiles,

		// TODO: Only send if updated
		SSHPolicy: sshPolicy,

		ControlTime: &now,

		Debug: &tailcfg.Debug{
			DisableLogTail:      !m.logtail,
			RandomizeClientPort: m.randomClientPort,
		},
	}

	log.Trace().
		Caller().
		Str("machine", mapRequest.Hostinfo.Hostname).
		// Interface("payload", resp).
		Msgf("Generated map response: %s", util.TailMapResponseToString(resp))

	return &resp, nil
}

func generateUserProfiles(
	machine *types.Machine,
	peers types.Machines,
	baseDomain string,
) []tailcfg.UserProfile {
	userMap := make(map[string]types.User)
	userMap[machine.User.Name] = machine.User
	for _, peer := range peers {
		userMap[peer.User.Name] = peer.User // not worth checking if already is there
	}

	profiles := []tailcfg.UserProfile{}
	for _, user := range userMap {
		displayName := user.Name

		if baseDomain != "" {
			displayName = fmt.Sprintf("%s@%s", user.Name, baseDomain)
		}

		profiles = append(profiles,
			tailcfg.UserProfile{
				ID:          tailcfg.UserID(user.ID),
				LoginName:   user.Name,
				DisplayName: displayName,
			})
	}

	return profiles
}

func generateDNSConfig(
	base *tailcfg.DNSConfig,
	baseDomain string,
	machine types.Machine,
	peers types.Machines,
) *tailcfg.DNSConfig {
	dnsConfig := base.Clone()

	// if MagicDNS is enabled
	if base != nil && base.Proxied {
		// Only inject the Search Domain of the current user
		// shared nodes should use their full FQDN
		dnsConfig.Domains = append(
			dnsConfig.Domains,
			fmt.Sprintf(
				"%s.%s",
				machine.User.Name,
				baseDomain,
			),
		)

		userSet := mapset.NewSet[types.User]()
		userSet.Add(machine.User)
		for _, p := range peers {
			userSet.Add(p.User)
		}
		for _, user := range userSet.ToSlice() {
			dnsRoute := fmt.Sprintf("%v.%v", user.Name, baseDomain)
			dnsConfig.Routes[dnsRoute] = nil
		}
	} else {
		dnsConfig = base
	}

	addNextDNSMetadata(dnsConfig.Resolvers, machine)

	return dnsConfig
}

// If any nextdns DoH resolvers are present in the list of resolvers it will
// take metadata from the machine metadata and instruct tailscale to add it
// to the requests. This makes it possible to identify from which device the
// requests come in the NextDNS dashboard.
//
// This will produce a resolver like:
// `https://dns.nextdns.io/<nextdns-id>?device_name=node-name&device_model=linux&device_ip=100.64.0.1`
func addNextDNSMetadata(resolvers []*dnstype.Resolver, machine types.Machine) {
	for _, resolver := range resolvers {
		if strings.HasPrefix(resolver.Addr, nextDNSDoHPrefix) {
			attrs := url.Values{
				"device_name":  []string{machine.Hostname},
				"device_model": []string{machine.HostInfo.OS},
			}

			if len(machine.IPAddresses) > 0 {
				attrs.Add("device_ip", machine.IPAddresses[0].String())
			}

			resolver.Addr = fmt.Sprintf("%s?%s", resolver.Addr, attrs.Encode())
		}
	}
}

func (m Mapper) CreateMapResponse(
	mapRequest tailcfg.MapRequest,
	machine *types.Machine,
	pol *policy.ACLPolicy,
) ([]byte, error) {
	mapResponse, err := m.fullMapResponse(mapRequest, machine, pol)
	if err != nil {
		return nil, err
	}

	if m.isNoise {
		return m.marshalMapResponse(mapResponse, key.MachinePublic{}, mapRequest.Compress)
	}

	var machineKey key.MachinePublic
	err = machineKey.UnmarshalText([]byte(util.MachinePublicKeyEnsurePrefix(machine.MachineKey)))
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot parse client key")

		return nil, err
	}

	return m.marshalMapResponse(mapResponse, machineKey, mapRequest.Compress)
}

func (m Mapper) CreateKeepAliveResponse(
	mapRequest tailcfg.MapRequest,
	machine *types.Machine,
) ([]byte, error) {
	keepAliveResponse := tailcfg.MapResponse{
		KeepAlive: true,
	}

	if m.isNoise {
		return m.marshalMapResponse(
			keepAliveResponse,
			key.MachinePublic{},
			mapRequest.Compress,
		)
	}

	var machineKey key.MachinePublic
	err := machineKey.UnmarshalText([]byte(util.MachinePublicKeyEnsurePrefix(machine.MachineKey)))
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot parse client key")

		return nil, err
	}

	return m.marshalMapResponse(keepAliveResponse, machineKey, mapRequest.Compress)
}

func MarshalResponse(
	resp interface{},
	privateKey2019 *key.MachinePrivate,
	machineKey key.MachinePublic,
) ([]byte, error) {
	jsonBody, err := json.Marshal(resp)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot marshal response")

		return nil, err
	}

	if privateKey2019 != nil {
		return privateKey2019.SealTo(machineKey, jsonBody), nil
	}

	return jsonBody, nil
}

func (m Mapper) marshalMapResponse(
	resp interface{},
	machineKey key.MachinePublic,
	compression string,
) ([]byte, error) {
	jsonBody, err := json.Marshal(resp)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot marshal map response")
	}

	var respBody []byte
	if compression == util.ZstdCompression {
		respBody = zstdEncode(jsonBody)
		if !m.isNoise { // if legacy protocol
			respBody = m.privateKey2019.SealTo(machineKey, respBody)
		}
	} else {
		if !m.isNoise { // if legacy protocol
			respBody = m.privateKey2019.SealTo(machineKey, jsonBody)
		} else {
			respBody = jsonBody
		}
	}

	data := make([]byte, reservedResponseHeaderSize)
	binary.LittleEndian.PutUint32(data, uint32(len(respBody)))
	data = append(data, respBody...)

	return data, nil
}

func zstdEncode(in []byte) []byte {
	encoder, ok := zstdEncoderPool.Get().(*zstd.Encoder)
	if !ok {
		panic("invalid type in sync pool")
	}
	out := encoder.EncodeAll(in, nil)
	_ = encoder.Close()
	zstdEncoderPool.Put(encoder)

	return out
}

var zstdEncoderPool = &sync.Pool{
	New: func() any {
		encoder, err := smallzstd.NewEncoder(
			nil,
			zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			panic(err)
		}

		return encoder
	},
}
