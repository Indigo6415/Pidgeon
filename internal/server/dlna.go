package server

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Indigo6415/pidgeon/internal/brand"
)

// TV mode (--tv) makes the served folder a DLNA / UPnP media server, so
// smart TVs, consoles, VLC and Kodi can browse and play its videos, music
// and pictures from their own menus. This file is the HTTP side: the device
// description, the ContentDirectory "Browse" action, and streaming (with
// Range, through the same path as every download). Announcing the server
// on the network (SSDP) is done by the ssdp package.
//
// TVs can't log in, so everything lives under /dlna/<key>/, where key is a
// random value only found in the announcements. Anyone on the network can
// therefore play the media files (nothing else) even with --token; --ask
// still makes every new device, TVs included, wait for approval.

// dlnaFlags advertises seeking by byte range and streaming transfer.
const dlnaFlags = "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000"

// dlnaMaxResults bounds one Browse response.
const dlnaMaxResults = 5000

// DLNA returns the server's UPnP device UUID and the path of its device
// description, or ok = false when TV mode is off.
func (s *Server) DLNA() (uuid, descPath string, ok bool) {
	if s.dlnaKey == "" {
		return "", "", false
	}
	return s.dlnaUUID, "/dlna/" + s.dlnaKey + "/desc.xml", true
}

// TVName is the name TVs list the server under.
func (s *Server) TVName() string {
	if s.cfg.TVName != "" {
		return s.cfg.TVName
	}
	return brand.Name + " · " + s.name
}

// TVSecretFile returns the default place for TV mode's secret:
// <user config dir>/<app>/tv.secret, or "" if there is no config dir.
func TVSecretFile(app string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, app, "tv.secret")
}

// tvSecret reads the kept secret, or makes (and keeps) a new one. Keeping
// it is best effort: without a file it lasts one run.
func tvSecret(file string) string {
	if file != "" {
		if b, err := os.ReadFile(file); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
			return strings.TrimSpace(string(b))
		}
	}
	secret := randomToken()
	if file != "" && os.MkdirAll(filepath.Dir(file), 0o700) == nil {
		_ = os.WriteFile(file, []byte(secret+"\n"), 0o600)
	}
	return secret
}

// setupDLNA picks the key and a UUID that stay the same for the same
// folders on the same machine, so TVs remember the server between runs.
// Samsung TVs, for one, keep using the address they first saw for a UUID.
func (s *Server) setupDLNA(mux *http.ServeMux) {
	host, _ := os.Hostname()
	folders := host + "\x00" + strings.Join(s.root.Paths(), "\x00")
	key := sha256.Sum256([]byte(tvSecret(s.cfg.TVSecretFile) + "\x00" + folders))
	s.dlnaKey = fmt.Sprintf("%x", key[:8])
	sum := sha256.Sum256([]byte(folders))
	sum[6] = sum[6]&0x0f | 0x50 // version 5 (name-based)
	sum[8] = sum[8]&0x3f | 0x80 // RFC 4122 variant
	s.dlnaUUID = fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])

	mux.HandleFunc("GET /dlna/{key}/desc.xml", s.dlnaGate(s.handleDLNADesc))
	mux.HandleFunc("GET /dlna/{key}/cds.xml", s.dlnaGate(xmlDoc(cdsSCPD)))
	mux.HandleFunc("GET /dlna/{key}/cms.xml", s.dlnaGate(xmlDoc(cmsSCPD)))
	mux.HandleFunc("POST /dlna/{key}/{svc}/control", s.dlnaGate(s.handleDLNAControl))
	mux.HandleFunc("SUBSCRIBE /dlna/{key}/{svc}/event", s.dlnaGate(handleDLNASubscribe))
	mux.HandleFunc("UNSUBSCRIBE /dlna/{key}/{svc}/event", s.dlnaGate(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("GET /dlna/{key}/f/{path...}", s.dlnaGate(s.handleDLNAFile))
	mux.HandleFunc("GET /dlna/{key}/thumb", s.dlnaGate(s.handleThumb))
}

// dlnaGate only lets requests with the right key through.
func (s *Server) dlnaGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !equal(r.PathValue("key"), s.dlnaKey) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Server", dlnaServerHeader())
		next(w, r)
	}
}

// dlnaServerHeader identifies the server the way UPnP asks for.
func dlnaServerHeader() string {
	return "Pidgeon/" + brand.ResolvedVersion() + " UPnP/1.0 DLNADOC/1.50"
}

// xmlDoc serves a fixed XML document.
func xmlDoc(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		io.WriteString(w, doc)
	}
}

// handleDLNADesc serves the device description.
func (s *Server) handleDLNADesc(w http.ResponseWriter, r *http.Request) {
	base := "/dlna/" + s.dlnaKey
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0" xmlns:sec="http://www.sec.co.kr/dlna">
<specVersion><major>1</major><minor>0</minor></specVersion>
<device>
<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
<sec:ProductCap>smi,DCM10,getMediaInfo.sec,getCaptionInfo.sec</sec:ProductCap>
<sec:X_ProductCap>smi,DCM10,getMediaInfo.sec,getCaptionInfo.sec</sec:X_ProductCap>
<friendlyName>%s</friendlyName>
<manufacturer>%s</manufacturer>
<modelName>%s</modelName>
<modelNumber>%s</modelNumber>
<UDN>uuid:%s</UDN>
<dlna:X_DLNADOC>DMS-1.50</dlna:X_DLNADOC>
<serviceList>
<service><serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType><serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId><SCPDURL>%[6]s/cds.xml</SCPDURL><controlURL>%[6]s/cds/control</controlURL><eventSubURL>%[6]s/cds/event</eventSubURL></service>
<service><serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType><serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId><SCPDURL>%[6]s/cms.xml</SCPDURL><controlURL>%[6]s/cms/control</controlURL><eventSubURL>%[6]s/cms/event</eventSubURL></service>
</serviceList>
</device>
</root>`, xesc(s.TVName()), xesc(brand.Name), xesc(brand.Name), xesc(brand.ResolvedVersion()), s.dlnaUUID, base)
}

// handleDLNASubscribe accepts event subscriptions. Nothing ever changes
// on our side that a TV must hear about, so no events are sent.
func handleDLNASubscribe(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get("SID")
	if sid == "" {
		sid = "uuid:" + randomToken()
	}
	w.Header().Set("SID", sid)
	w.Header().Set("TIMEOUT", "Second-1800")
}

// handleDLNAFile streams a media file with the headers DLNA players need.
func (s *Server) handleDLNAFile(w http.ResponseWriter, r *http.Request) {
	name, err := s.resolve(r.PathValue("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	kind, typ := mediaKind(name)
	if kind == "" {
		http.Error(w, "not a media file", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", typ)
	h.Set("contentFeatures.dlna.org", dlnaFlags)
	h.Set("transferMode.dlna.org", map[bool]string{true: "Interactive", false: "Streaming"}[kind == "image"])
	s.serveFile(w, r, name, "")
}

// soapArgs collects the text of every leaf element of a SOAP request body
// by local name (ObjectID, BrowseFlag, ...).
func soapArgs(body io.Reader) map[string]string {
	out := map[string]string{}
	dec := xml.NewDecoder(io.LimitReader(body, 64<<10))
	var cur string
	var text strings.Builder
	// Walk the tokens; a closing tag ends the element whose text we hold.
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			cur = t.Name.Local
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if t.Name.Local == cur {
				out[cur] = strings.TrimSpace(text.String())
			}
			cur = ""
		}
	}
}

// handleDLNAControl answers SOAP actions for both services.
func (s *Server) handleDLNAControl(w http.ResponseWriter, r *http.Request) {
	svc := r.PathValue("svc")
	// SOAPACTION: "urn:schemas-upnp-org:service:ContentDirectory:1#Browse"
	action := strings.Trim(r.Header.Get("SOAPACTION"), `" `)
	_, action, _ = strings.Cut(action, "#")
	args := soapArgs(r.Body)
	switch svc + "#" + action {
	case "cds#Browse":
		s.dlnaBrowse(w, r, args)
	case "cds#GetSearchCapabilities":
		soapReply(w, "ContentDirectory", action, "SearchCaps", "")
	case "cds#GetSortCapabilities":
		soapReply(w, "ContentDirectory", action, "SortCaps", "")
	case "cds#GetSystemUpdateID":
		soapReply(w, "ContentDirectory", action, "Id", "1")
	case "cds#X_GetFeatureList":
		// Samsung TVs ask where videos, music and pictures start before
		// browsing; everything starts at the root.
		soapReply(w, "ContentDirectory", action, "FeatureList", samsungFeatures)
	case "cms#GetProtocolInfo":
		soapReply(w, "ConnectionManager", action, "Source", protocolInfoList(), "Sink", "")
	case "cms#GetCurrentConnectionIDs":
		soapReply(w, "ConnectionManager", action, "ConnectionIDs", "0")
	case "cms#GetCurrentConnectionInfo":
		soapReply(w, "ConnectionManager", action, "RcsID", "-1", "AVTransportID", "-1", "ProtocolInfo", "",
			"PeerConnectionManager", "", "PeerConnectionID", "-1", "Direction", "Output", "Status", "OK")
	default:
		soapFault(w, 401, "Invalid Action")
	}
}

// protocolInfoList is every format we serve, for GetProtocolInfo.
func protocolInfoList() string {
	seen := map[string]bool{}
	var out []string
	for _, t := range mediaTypes {
		if !seen[t] {
			seen[t] = true
			out = append(out, "http-get:*:"+t+":*")
		}
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// soapReply writes an action's response; kv are its output arguments in order.
func soapReply(w http.ResponseWriter, service, action string, kv ...string) {
	var sb strings.Builder
	// Arguments come in name/value pairs.
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&sb, "<%s>%s</%[1]s>", kv[i], xesc(kv[i+1]))
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("EXT", "")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%sResponse xmlns:u="urn:schemas-upnp-org:service:%s:1">%s</u:%[1]sResponse></s:Body></s:Envelope>`,
		action, service, sb.String())
}

// soapFault reports a UPnP error.
func soapFault(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`,
		code, xesc(desc))
}

// samsungFeatures answers Samsung's X_GetFeatureList.
const samsungFeatures = `<?xml version="1.0" encoding="UTF-8"?><Features xmlns="urn:schemas-upnp-org:av:avs" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="urn:schemas-upnp-org:av:avs http://www.upnp.org/schemas/av/avs.xsd"><Feature name="samsung.com_BASICVIEW" version="1"><container id="0" type="object.item.audioItem"/><container id="0" type="object.item.videoItem"/><container id="0" type="object.item.imageItem"/></Feature></Features>`

// xmlQuotes swaps the numeric quote escapes of xml.EscapeText for the
// named ones: some TVs' XML parsers only know the named entities.
var xmlQuotes = strings.NewReplacer("&#34;", "&quot;", "&#39;", "&apos;")

// xesc escapes text for XML.
func xesc(s string) string {
	var sb strings.Builder
	xml.EscapeText(&sb, []byte(s))
	return xmlQuotes.Replace(sb.String())
}

// dlnaID maps a root-relative name to a DLNA object ID: "0" is the root,
// everything else is "/" plus its path.
func dlnaID(name string) string {
	if name == "." {
		return "0"
	}
	return "/" + name
}

// dlnaName maps an object ID back to a root-relative name.
func (s *Server) dlnaName(id string) (string, error) {
	if id == "0" || id == "" {
		return ".", nil
	}
	return s.resolve(id)
}

// dlnaBrowse answers Browse: the object itself (BrowseMetadata) or a page
// of a folder's subfolders and media files (BrowseDirectChildren).
func (s *Server) dlnaBrowse(w http.ResponseWriter, r *http.Request, args map[string]string) {
	name, err := s.dlnaName(args["ObjectID"])
	if err != nil {
		soapFault(w, 701, "No such object")
		return
	}
	fi, err := s.stat(name)
	if err != nil {
		soapFault(w, 701, "No such object")
		return
	}
	base := "http://" + r.Host + "/dlna/" + s.dlnaKey
	var objs []string
	total := 0
	if args["BrowseFlag"] == "BrowseMetadata" {
		if fi.IsDir() {
			objs = append(objs, s.didlContainer(name))
		} else if o := s.didlItem(base, name, fi.Size(), fi.ModTime()); o != "" {
			objs = append(objs, o)
		}
		total = len(objs)
	} else {
		if !fi.IsDir() {
			soapFault(w, 710, "No such container")
			return
		}
		all, err := s.dlnaChildren(name)
		if err != nil {
			soapFault(w, 701, "No such object")
			return
		}
		total = len(all)
		start, _ := strconv.Atoi(args["StartingIndex"])
		count, _ := strconv.Atoi(args["RequestedCount"])
		start = min(max(0, start), total)
		if count <= 0 || count > dlnaMaxResults {
			count = dlnaMaxResults
		}
		// One DIDL object per entry on the requested page.
		for _, e := range all[start:min(total, start+count)] {
			p := path.Join(name, e.Name)
			if e.Dir {
				objs = append(objs, s.didlContainer(p))
			} else {
				objs = append(objs, s.didlItem(base, p, e.Size, time.UnixMilli(e.MTime)))
			}
		}
	}
	didl := `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/">` +
		strings.Join(objs, "") + `</DIDL-Lite>`
	soapReply(w, "ContentDirectory", "Browse", "Result", didl, "NumberReturned", strconv.Itoa(len(objs)),
		"TotalMatches", strconv.Itoa(total), "UpdateID", "1")
}

// dlnaChildren lists what a TV sees in a folder: its subfolders, then its
// media files, each in natural order.
func (s *Server) dlnaChildren(name string) ([]entry, error) {
	es, err := s.list(name)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(es, func(a, b entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return naturalCompare(a.Name, b.Name)
	})
	var out []entry
	// Keep folders and playable files; documents and archives are left out.
	for _, e := range es {
		if k, _ := mediaKind(e.Name); e.Dir || (k != "" && !e.Archive) {
			out = append(out, e)
		}
	}
	return out, nil
}

// dlnaParent is the object ID of a name's folder.
func dlnaParent(name string) string {
	if name == "." {
		return "-1"
	}
	return dlnaID(path.Dir(name))
}

// didlContainer describes a folder. Many TVs read a missing childCount as
// an empty folder and never browse into it, so it is always given.
func (s *Server) didlContainer(name string) string {
	title := s.TVName()
	if name != "." {
		title = path.Base(name)
	}
	kids, _ := s.dlnaChildren(name)
	return fmt.Sprintf(`<container id="%s" parentID="%s" childCount="%d" restricted="1" searchable="0"><dc:title>%s</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`,
		xesc(dlnaID(name)), xesc(dlnaParent(name)), len(kids), xesc(title))
}

// didlItem describes a media file, or returns "" for anything else.
func (s *Server) didlItem(base, name string, size int64, mtime time.Time) string {
	kind, typ := mediaKind(name)
	class := map[string]string{"video": "object.item.videoItem", "audio": "object.item.audioItem.musicTrack", "image": "object.item.imageItem.photo"}[kind]
	if class == "" {
		return ""
	}
	file := path.Base(name)
	src := base + "/f/" + escapePath(name)
	art := ""
	// Pictures come with a small preview the TV can show in its grid.
	if kind == "image" && s.thumbs != nil && thumbExts[strings.ToLower(path.Ext(name))] {
		art = `<upnp:albumArtURI dlna:profileID="JPEG_TN">` + xesc(base+"/thumb?w=256&path="+url.QueryEscape("/"+name)) + `</upnp:albumArtURI>`
	}
	return fmt.Sprintf(`<item id="%s" parentID="%s" restricted="1"><dc:title>%s</dc:title><upnp:class>%s</upnp:class><dc:date>%s</dc:date>%s<res protocolInfo="http-get:*:%s:%s" size="%d">%s</res></item>`,
		xesc(dlnaID(name)), xesc(dlnaParent(name)), xesc(strings.TrimSuffix(file, path.Ext(file))), class,
		mtime.Format("2006-01-02T15:04:05"), art, typ, dlnaFlags, size, xesc(src))
}

// cdsSCPD describes the ContentDirectory service.
const cdsSCPD = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
<actionList>
<action><name>Browse</name><argumentList>
<argument><name>ObjectID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
<argument><name>BrowseFlag</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable></argument>
<argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
<argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
<argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
<argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
<argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
<argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
<argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
<argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable></argument>
</argumentList></action>
<action><name>GetSearchCapabilities</name><argumentList><argument><name>SearchCaps</name><direction>out</direction><relatedStateVariable>SearchCapabilities</relatedStateVariable></argument></argumentList></action>
<action><name>GetSortCapabilities</name><argumentList><argument><name>SortCaps</name><direction>out</direction><relatedStateVariable>SortCapabilities</relatedStateVariable></argument></argumentList></action>
<action><name>GetSystemUpdateID</name><argumentList><argument><name>Id</name><direction>out</direction><relatedStateVariable>SystemUpdateID</relatedStateVariable></argument></argumentList></action>
<action><name>X_GetFeatureList</name><argumentList><argument><name>FeatureList</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Featurelist</relatedStateVariable></argument></argumentList></action>
</actionList>
<serviceStateTable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ObjectID</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_BrowseFlag</name><dataType>string</dataType><allowedValueList><allowedValue>BrowseMetadata</allowedValue><allowedValue>BrowseDirectChildren</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Filter</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Index</name><dataType>ui4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Count</name><dataType>ui4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_SortCriteria</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Result</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_UpdateID</name><dataType>ui4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>SearchCapabilities</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>SortCapabilities</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Featurelist</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="yes"><name>SystemUpdateID</name><dataType>ui4</dataType></stateVariable>
</serviceStateTable>
</scpd>`

// cmsSCPD describes the ConnectionManager service.
const cmsSCPD = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
<actionList>
<action><name>GetProtocolInfo</name><argumentList>
<argument><name>Source</name><direction>out</direction><relatedStateVariable>SourceProtocolInfo</relatedStateVariable></argument>
<argument><name>Sink</name><direction>out</direction><relatedStateVariable>SinkProtocolInfo</relatedStateVariable></argument>
</argumentList></action>
<action><name>GetCurrentConnectionIDs</name><argumentList><argument><name>ConnectionIDs</name><direction>out</direction><relatedStateVariable>CurrentConnectionIDs</relatedStateVariable></argument></argumentList></action>
<action><name>GetCurrentConnectionInfo</name><argumentList>
<argument><name>ConnectionID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
<argument><name>RcsID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_RcsID</relatedStateVariable></argument>
<argument><name>AVTransportID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_AVTransportID</relatedStateVariable></argument>
<argument><name>ProtocolInfo</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ProtocolInfo</relatedStateVariable></argument>
<argument><name>PeerConnectionManager</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionManager</relatedStateVariable></argument>
<argument><name>PeerConnectionID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
<argument><name>Direction</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Direction</relatedStateVariable></argument>
<argument><name>Status</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionStatus</relatedStateVariable></argument>
</argumentList></action>
</actionList>
<serviceStateTable>
<stateVariable sendEvents="yes"><name>SourceProtocolInfo</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="yes"><name>SinkProtocolInfo</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="yes"><name>CurrentConnectionIDs</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionStatus</name><dataType>string</dataType><allowedValueList><allowedValue>OK</allowedValue><allowedValue>ContentFormatMismatch</allowedValue><allowedValue>InsufficientBandwidth</allowedValue><allowedValue>UnreliableChannel</allowedValue><allowedValue>Unknown</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionManager</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Direction</name><dataType>string</dataType><allowedValueList><allowedValue>Input</allowedValue><allowedValue>Output</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ProtocolInfo</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionID</name><dataType>i4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_AVTransportID</name><dataType>i4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_RcsID</name><dataType>i4</dataType></stateVariable>
</serviceStateTable>
</scpd>`
