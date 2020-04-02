// Package hilink provides a Hilink WebUI client.
package hilink

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clbanning/mxj"
)

// see: https://blog.hqcodeshop.fi/archives/259-Huawei-E5186-AJAX-API.html
// also see: https://github.com/BlackyPanther/Huawei-HiLink/blob/master/hilink.class.php
// also see: http://www.bez-kabli.pl/viewtopic.php?t=42168

const (
	// DefaultURL is the default URL endpoint for the Hilink WebUI.
	DefaultURL = "http://192.168.8.1/"

	// DefaultTimeout is the default timeout.
	DefaultTimeout = 30 * time.Second

	// TokenHeader is the header used by the WebUI for CSRF tokens.
	TokenHeader = "__RequestVerificationToken"
)

// Client represents a Hilink client connection.
type Client struct {
	rawurl    string
	url       *url.URL
	authID    string
	authPW    string
	nostart   bool
	client    *http.Client
	token     string
	transport http.RoundTripper

	sync.Mutex
}

// NewClient creates a new client a Hilink device.
func NewClient(opts ...Option) (*Client, error) {
	var err error

	// create client
	c := &Client{
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}

	// process options
	for _, o := range opts {
		err = o(c)
		if err != nil {
			return nil, err
		}
	}

	// set default url
	if c.rawurl == "" || c.url == nil {
		err = URL(DefaultURL)(c)
		if err != nil {
			return nil, err
		}
	}

	// start session
	if !c.nostart {
		// retrieve session id
		sessID, tokID, err := c.NewSessionAndTokenID()
		if err != nil {
			return nil, err
		}

		// set session id
		err = c.SetSessionAndTokenID(sessID, tokID)
		if err != nil {
			return nil, err
		}

		// try login, ignore the OK value
		_, err = c.login()
		if err != nil {
			return nil, err
		}
	}

	return c, nil
}

// createRequest creates a request for use with the Client.
func (c *Client) createRequest(urlstr string, v interface{}) (*http.Request, error) {
	if v == nil {
		return http.NewRequest("GET", urlstr, nil)
	}

	// encode xml
	body, err := encodeXML(v)
	if err != nil {
		return nil, err
	}

	// build req
	req, err := http.NewRequest("POST", urlstr, body)
	if err != nil {
		return nil, err
	}

	// set content type and CSRF token
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set(TokenHeader, c.token)

	return req, nil
}

// doReq sends a request to the server with the provided path. If data is nil,
// then GET will be used as the HTTP method, otherwise POST will be used.
func (c *Client) doReq(path string, v interface{}, takeFirstEl bool) (interface{}, error) {
	c.Lock()
	defer c.Unlock()

	var err error

	// create http request
	q, err := c.createRequest(c.rawurl+path, v)
	if err != nil {
		return nil, err
	}

	// do request
	r, err := c.client.Do(q)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()

	// check status code
	if r.StatusCode != http.StatusOK {
		return nil, ErrBadStatusCode
	}

	// retrieve and save csrf token header
	tok := r.Header.Get(TokenHeader)
	if tok != "" {
		c.token = tok
	}

	// read body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	// decode
	m, err := decodeXML(body, takeFirstEl)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// doReqString wraps a request operation, returning the data of the specified
// child node named elName as a string.
func (c *Client) doReqString(path string, v interface{}, elName string) (string, error) {
	// send request
	res, err := c.doReq(path, v, true)
	if err != nil {
		return "", err
	}

	// convert
	d, ok := res.(map[string]interface{})
	if !ok {
		return "", ErrInvalidXML
	}

	l, ok := d[elName]
	if !ok {
		return "", ErrInvalidResponse
	}

	s, ok := l.(string)
	if !ok {
		return "", ErrInvalidValue
	}

	return s, nil
}

// doReqCheckOK wraps a request operation (ie, connect, disconnect, etc),
// checking success via the presence of 'OK' in the XML <response/>.
func (c *Client) doReqCheckOK(path string, v interface{}) (bool, error) {
	res, err := c.doReq(path, v, false)
	if err != nil {
		return false, err
	}

	// expect mxj.Map
	m, ok := res.(mxj.Map)
	if !ok {
		return false, ErrInvalidResponse
	}

	// check response present
	o := map[string]interface{}(m)
	r, ok := o["response"]
	if !ok {
		return false, ErrInvalidResponse
	}

	// convert
	s, ok := r.(string)
	if !ok {
		return false, ErrInvalidValue
	}

	return s == "OK", nil
}

// login authentifies the user using the user identifier and password given
// with the Auth option. Return nil if succeeded, or no Auth option
// was given, or the identifier is an empty string.
func (c *Client) login() (bool, error) {
	if c.authID == "" {
		return false, nil
	}
	// encode hashed password
	h := sha256.Sum256([]byte(c.authPW + c.token))
	tokenizedPW := base64.RawStdEncoding.EncodeToString([]byte(hex.EncodeToString(h[:])))
	return c.doReqCheckOK("api/user/login", XMLData{
		"Username":      c.authID,
		"Password":      tokenizedPW,
		"password_type": 4,
	})
}

// Do sends a request to the server with the provided path. If data is nil,
// then GET will be used as the HTTP method, otherwise POST will be used.
func (c *Client) Do(path string, v interface{}) (XMLData, error) {
	// send request
	res, err := c.doReq(path, v, true)
	if err != nil {
		return nil, err
	}

	// convert
	d, ok := res.(map[string]interface{})
	if !ok {
		return nil, ErrInvalidXML
	}

	return d, nil
}

// NewSessionAndTokenID starts a session with the server, and returns the
// session and token.
func (c *Client) NewSessionAndTokenID() (string, string, error) {
	res, err := c.doReq("api/webserver/SesTokInfo", nil, true)
	if err != nil {
		return "", "", err
	}

	// convert
	vals, ok := res.(map[string]interface{})
	if !ok {
		return "", "", ErrInvalidResponse
	}

	// check ses/tokInfo present
	sesInfo, ok := vals["SesInfo"]
	if !ok {
		return "", "", ErrInvalidResponse
	}
	tokInfo, ok := vals["TokInfo"]
	if !ok {
		return "", "", ErrInvalidResponse
	}

	// convert to strings
	s, ok := sesInfo.(string)
	if !ok {
		return "", "", ErrInvalidResponse
	}
	t, ok := tokInfo.(string)
	if !ok {
		return "", "", ErrInvalidResponse
	}

	return strings.TrimPrefix(s, "SessionID="), t, nil
}

// SetSessionAndTokenID sets the sessionID and tokenID for the Client.
func (c *Client) SetSessionAndTokenID(sessionID, tokenID string) error {
	c.Lock()
	defer c.Unlock()

	var err error

	// create cookie jar
	c.client.Jar, err = cookiejar.New(nil)
	if err != nil {
		return err
	}

	// set values on client
	c.client.Jar.SetCookies(c.url, []*http.Cookie{&http.Cookie{
		Name:  "SessionID",
		Value: sessionID,
	}})
	c.token = tokenID

	return nil
}

// GlobalConfig retrieves global Hilink configuration.
func (c *Client) GlobalConfig() (XMLData, error) {
	return c.Do("config/global/config.xml", nil)
}

// NetworkTypes retrieves available network types.
func (c *Client) NetworkTypes() (XMLData, error) {
	return c.Do("config/global/net-type.xml", nil)
}

// PCAssistantConfig retrieves PC Assistant configuration.
func (c *Client) PCAssistantConfig() (XMLData, error) {
	return c.Do("config/pcassistant/config.xml", nil)
}

// DeviceConfig retrieves device configuration.
func (c *Client) DeviceConfig() (XMLData, error) {
	return c.Do("config/deviceinformation/config.xml", nil)
}

// WebUIConfig retrieves WebUI configuration.
func (c *Client) WebUIConfig() (XMLData, error) {
	return c.Do("config/webuicfg/config.xml", nil)
}

// SmsConfig retrieves device SMS configuration.
func (c *Client) SmsConfig() (XMLData, error) {
	return c.Do("api/sms/config", nil)
}

// WlanConfig retrieves basic WLAN settings.
func (c *Client) WlanConfig() (XMLData, error) {
	return c.Do("api/wlan/basic-settings", nil)
}

// DhcpConfig retrieves DHCP configuration.
func (c *Client) DhcpConfig() (XMLData, error) {
	return c.Do("api/dhcp/settings", nil)
}

// CradleStatusInfo retrieves cradle status information.
func (c *Client) CradleStatusInfo() (XMLData, error) {
	return c.Do("api/cradle/status-info", nil)
}

// CradleMACSet sets the MAC address for the cradle.
func (c *Client) CradleMACSet(addr string) (bool, error) {
	return c.doReqCheckOK("api/cradle/current-mac", XMLData{
		"currentmac": addr,
	})
}

// CradleMAC retrieves cradle MAC address.
func (c *Client) CradleMAC() (string, error) {
	return c.doReqString("api/cradle/current-mac", nil, "currentmac")
}

// AutorunVersion retrieves device autorun version.
func (c *Client) AutorunVersion() (string, error) {
	return c.doReqString("api/device/autorun-version", nil, "Version")
}

// DeviceBasicInfo retrieves basic device information.
func (c *Client) DeviceBasicInfo() (XMLData, error) {
	return c.Do("api/device/basic_information", nil)
}

// PublicKey retrieves webserver public key.
func (c *Client) PublicKey() (string, error) {
	return c.doReqString("api/webserver/publickey", nil, "encpubkeyn")
}

// DeviceControl sends a control code to the device.
func (c *Client) DeviceControl(code uint) (bool, error) {
	return c.doReqCheckOK("api/device/control", XMLData{
		"Control": fmt.Sprintf("%d", code),
	})
}

// DeviceReboot restarts the device.
func (c *Client) DeviceReboot() (bool, error) {
	return c.DeviceControl(1)
}

// DeviceReset resets the device configuration.
func (c *Client) DeviceReset() (bool, error) {
	return c.DeviceControl(2)
}

// DeviceBackup backups device configuration and retrieves backed up
// configuration data as a base64 encoded string.
func (c *Client) DeviceBackup() (string, error) {
	// cause backup to be generated
	ok, err := c.DeviceControl(3)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("unable to backup device configuration")
	}

	// retrieve data
	//res, err := c.doReq("nvram.bak")
	return " -- not implemented -- ", nil
}

// DeviceShutdown shuts down the device.
func (c *Client) DeviceShutdown() (bool, error) {
	return c.DeviceControl(4)
}

// DeviceFeatures retrieves device feature information.
func (c *Client) DeviceFeatures() (XMLData, error) {
	return c.Do("api/device/device-feature-switch", nil)
}

// DeviceInfo retrieves general device information.
func (c *Client) DeviceInfo() (XMLData, error) {
	return c.Do("api/device/information", nil)
}

// DeviceModeSet sets the device mode (0-project, 1-debug).
func (c *Client) DeviceModeSet(mode uint) (bool, error) {
	return c.doReqCheckOK("api/device/mode", XMLData{
		"mode": fmt.Sprintf("%d", mode),
	})
}

// FastbootFeatures retrieves fastboot feature information.
func (c *Client) FastbootFeatures() (XMLData, error) {
	return c.Do("api/device/fastbootswitch", nil)
}

// PowerFeatures retrieves power feature information.
func (c *Client) PowerFeatures() (XMLData, error) {
	return c.Do("api/device/powersaveswitch", nil)
}

// TetheringFeatures retrieves USB tethering feature information.
func (c *Client) TetheringFeatures() (XMLData, error) {
	return c.Do("api/device/usb-tethering-switch", nil)
}

// SignalInfo retrieves network signal information.
func (c *Client) SignalInfo() (XMLData, error) {
	return c.Do("api/device/signal", nil)
}

// ConnectionInfo retrieves connection (dialup) information.
func (c *Client) ConnectionInfo() (XMLData, error) {
	return c.Do("api/dialup/connection", nil)
}

// doReqConn wraps a connection manipulation request.
func (c *Client) ConnectionProfile(roaming, maxIdleTime string,

// connectMode, autoReconnect, roamAutoConnect, roamAutoReconnect string,
// interval, idle int,
) (bool, error) {
	return c.doReqCheckOK("api/dialup/connection", SimpleRequestXML(
		"ConnectMode", "0",
		"MTU", "1500",
		"MaxIdelTime", maxIdleTime,
		"RoamAutoConnectEnable", roaming,
		"auto_dial_switch", "1",
		"pdp_always_on", "0",
	))
}

// GlobalFeatures retrieves global feature information.
func (c *Client) GlobalFeatures() (XMLData, error) {
	return c.Do("api/global/module-switch", nil)
}

// Language retrieves current language.
func (c *Client) Language() (string, error) {
	return c.doReqString("api/language/current-language", nil, "CurrentLanguage")
}

// LanguageSet sets the language.
func (c *Client) LanguageSet(lang string) (bool, error) {
	return c.doReqCheckOK("api/language/current-language", XMLData{
		"CurrentLanguage": lang,
	})
}

// NotificationInfo retrieves notification information.
func (c *Client) NotificationInfo() (XMLData, error) {
	return c.Do("api/monitoring/check-notifications", nil)
}

// SimInfo retrieves SIM card information.
func (c *Client) SimInfo() (XMLData, error) {
	return c.Do("api/monitoring/converged-status", nil)
}

// StatusInfo retrieves general device status information.
func (c *Client) StatusInfo() (XMLData, error) {
	return c.Do("api/monitoring/status", nil)
}

// TrafficInfo retrieves traffic statistic information.
func (c *Client) TrafficInfo() (XMLData, error) {
	return c.Do("api/monitoring/traffic-statistics", nil)
}

// TrafficClear clears the current traffic statistics.
func (c *Client) TrafficClear() (bool, error) {
	return c.doReqCheckOK("api/monitoring/clear-traffic", XMLData{
		"ClearTraffic": "1",
	})
}

// MonthInfo retrieves the month download statistic information.
func (c *Client) MonthInfo() (XMLData, error) {
	return c.Do("api/monitoring/month_statistics", nil)
}

// WlanMonthInfo retrieves the WLAN month download statistic information.
func (c *Client) WlanMonthInfo() (XMLData, error) {
	return c.Do("api/monitoring/month_statistics_wlan", nil)
}

// NetworkInfo retrieves network provider information.
func (c *Client) NetworkInfo() (XMLData, error) {
	return c.Do("api/net/current-plmn", nil)
}

// WifiFeatures retrieves wifi feature information.
func (c *Client) WifiFeatures() (XMLData, error) {
	return c.Do("api/wlan/wifi-feature-switch", nil)
}

// ModeList retrieves available network modes.
func (c *Client) ModeList() (XMLData, error) {
	return c.Do("api/net/net-mode-list", nil)
}

// ModeInfo retrieves network mode settings information.
func (c *Client) ModeInfo() (XMLData, error) {
	return c.Do("api/net/net-mode", nil)
}

// ModeNetworkInfo retrieves current network mode information.
func (c *Client) ModeNetworkInfo() (XMLData, error) {
	return c.Do("api/net/network", nil)
}

// ModeSet sets the network mode.
func (c *Client) ModeSet(netMode, netBand, lteBand string) (bool, error) {
	return c.doReqCheckOK("api/net/net-mode", SimpleRequestXML(
		"NetworkMode", netMode,
		"NetworkBand", netBand,
		"LTEBand", lteBand,
	))
}

// PinInfo retrieves SIM PIN status information.
func (c *Client) PinInfo() (XMLData, error) {
	return c.Do("api/pin/status", nil)
}

// doReqPin wraps a SIM PIN manipulation request.
func (c *Client) doReqPin(pt PinType, cur, new, puk string) (bool, error) {
	return c.doReqCheckOK("api/pin/operate", SimpleRequestXML(
		"OperateType", fmt.Sprintf("%d", pt),
		"CurrentPin", cur,
		"NewPin", new,
		"PukCode", puk,
	))
}

// PinEnter enters a SIM PIN.
func (c *Client) PinEnter(pin string) (bool, error) {
	return c.doReqPin(PinTypeEnter, pin, "", "")
}

// PinActivate activates a SIM PIN.
func (c *Client) PinActivate(pin string) (bool, error) {
	return c.doReqPin(PinTypeActivate, pin, "", "")
}

// PinDeactivate deactivates a SIM PIN.
func (c *Client) PinDeactivate(pin string) (bool, error) {
	return c.doReqPin(PinTypeDeactivate, pin, "", "")
}

// PinChange changes a SIM PIN.
func (c *Client) PinChange(pin, new string) (bool, error) {
	return c.doReqPin(PinTypeChange, pin, new, "")
}

// PinEnterPuk enters a SIM PIN puk.
func (c *Client) PinEnterPuk(puk, new string) (bool, error) {
	return c.doReqPin(PinTypeEnterPuk, new, new, puk)
}

// PinSaveInfo retrieves SIM PIN save information.
func (c *Client) PinSaveInfo() (XMLData, error) {
	return c.Do("api/pin/save-pin", nil)
}

// PinSimlockInfo retrieves SIM lock information.
func (c *Client) PinSimlockInfo() (XMLData, error) {
	return c.Do("api/pin/simlock", nil)
}

func (c *Client) MobileDataSwitch() (XMLData, error) {
	return c.Do("api/dialup/mobile-dataswitch", nil)
}

func (c *Client) MobileDataSwitchState(state string) (bool, error) {
	return c.doReqCheckOK("api/dialup/mobile-dataswitch", XMLData{
		"dataswitch": state,
	})
}

func (c *Client) MobileDataActivate() (bool, error) {
	return c.doReqCheckOK("api/dialup/mobile-dataswitch", XMLData{
		"dataswitch": "1",
	})
}

func (c *Client) MobileDataDeactivate() (bool, error) {
	return c.doReqCheckOK("api/dialup/mobile-dataswitch", XMLData{
		"dataswitch": "0",
	})
}

// Connect connects the Hilink device to the network provider.
func (c *Client) Connect() (bool, error) {
	return c.doReqCheckOK("api/dialup/dial", XMLData{
		"Action": "1",
	})
}

// Disconnect disconnects the Hilink device from the network provider.
func (c *Client) Disconnect() (bool, error) {
	return c.doReqCheckOK("api/dialup/dial", XMLData{
		"Action": "0",
	})
}

// ProfileInfo retrieves profile information (ie, APN).
// func (c *Client) setRoaming(active bool) (XMLData, error) {
// 	return c.doReqCheckOK("api/dialup/connection", SimpleRequestXML(
// 	))
// }

// ProfileInfo retrieves profile information (ie, APN).
func (c *Client) ProfileInfo() (XMLData, error) {
	return c.Do("api/dialup/profiles", nil)
}

// Add connection profile
func (c *Client) ProfileAdd(name string, apn string, user string, password string, isDefault bool) (bool, error) {
	var newDefaultValue string
	if isDefault {
		newDefaultValue = "0"
	} else {
		newDefaultValue = "1"
	}
	return c.doReqCheckOK("api/dialup/profiles", XMLData{
		"Delete":     0,
		"SetDefault": newDefaultValue,
		"Modify":     1,
		"Profile": XMLData{
			"Index":        "", //original is new_index
			"IsValid":      1,
			"Name":         name,
			"ApnIsStatic":  "1",
			"ApnName":      apn,
			"DialupNum":    "*99#",
			"Username":     user,
			"Password":     password,
			"AuthMode":     "0",
			"IpIsStatic":   "",
			"IpAddress":    "",
			"DnsIsStatic":  "",
			"PrimaryDns":   "",
			"SecondaryDns": "",
			"ReadOnly":     "0",
			"iptype":       "0",
		},
	})
}

// Delete connection profile
func (c *Client) ProfileDelete(index, newDefault string) (bool, error) {
	return c.doReqCheckOK("api/dialup/profiles", SimpleRequestXML(
		"Delete", index,
		"SetDefault", newDefault,
		"Modify", "0",
	))
}

// SmsFeatures retrieves SMS feature information.
func (c *Client) SmsFeatures() (XMLData, error) {
	return c.Do("api/sms/sms-feature-switch", nil)
}

// SmsList retrieves list of SMS in an inbox.
func (c *Client) SmsList(boxType, page, count uint, sortByName, ascending, unreadPreferred bool) (XMLData, error) {
	// execute request -- note: the order is important!
	return c.Do("api/sms/sms-list", SimpleRequestXML(
		"PageIndex", fmt.Sprintf("%d", page),
		"ReadCount", fmt.Sprintf("%d", count),
		"BoxType", fmt.Sprintf("%d", boxType),
		"SortType", boolToString(sortByName),
		"Ascending", boolToString(ascending),
		"UnreadPreferred", boolToString(unreadPreferred),
	))
}

// SmsCount retrieves count of SMS per inbox type.
func (c *Client) SmsCount() (XMLData, error) {
	return c.Do("api/sms/sms-count", nil)
}

// SmsSend sends an SMS.
func (c *Client) SmsSend(msg string, to ...string) (bool, error) {
	if len(msg) >= 160 {
		return false, ErrMessageTooLong
	}

	// build phones
	phones := []string{}
	for _, t := range to {
		phones = append(phones, "Phone", t)
	}

	// send request (order matters below!)
	return c.doReqCheckOK("api/sms/send-sms", SimpleRequestXML(
		"Index", "-1",
		"Phones", "\n"+string(xmlPairs("    ", phones...)),
		"Sca", "",
		"Content", msg,
		"Length", fmt.Sprintf("%d", len(msg)),
		"Reserved", "1",
		"Date", time.Now().Format("2006-01-02 15:04:05"),
	))
}

// SmsSendStatus retrieves SMS send status information.
func (c *Client) SmsSendStatus() (XMLData, error) {
	return c.Do("api/sms/send-status", nil)
}

// SmsReadSet sets the read status of a SMS.
func (c *Client) SmsReadSet(id string) (bool, error) {
	return c.doReqCheckOK("api/sms/set-read", SimpleRequestXML(
		"Index", id,
	))
}

// SmsDelete deletes a specified SMS.
func (c *Client) SmsDelete(id string) (bool, error) {
	return c.doReqCheckOK("api/sms/delete-sms", SimpleRequestXML(
		"Index", id,
	))
}

// UssdStatus retrieves current USSD session status information.
func (c *Client) UssdStatus() (UssdState, error) {
	s, err := c.doReqString("api/ussd/status", nil, "result")
	if err != nil {
		return UssdStateNone, err
	}

	i, err := strconv.Atoi(s)
	if err != nil {
		return UssdStateNone, ErrInvalidResponse
	}

	return UssdState(i), nil
}

// UssdCode sends a USSD code to the Hilink device.
func (c *Client) UssdCode(code string) (bool, error) {
	return c.doReqCheckOK("api/ussd/send", SimpleRequestXML(
		"content", code,
		"codeType", "CodeType",
		"timeout", "",
	))
}

// UssdContent retrieves content buffer of the active USSD session.
func (c *Client) UssdContent() (string, error) {
	return c.doReqString("api/ussd/get", nil, "content")
}

// UssdRelease releases the active USSD session.
func (c *Client) UssdRelease() (bool, error) {
	return c.doReqCheckOK("api/ussd/release", nil)
}

// DdnsList retrieves list of DDNS providers.
func (c *Client) DdnsList() (XMLData, error) {
	return c.Do("api/ddns/ddns-list", nil)
}

// LogPath retrieves device log path (URL).
func (c *Client) LogPath() (string, error) {
	return c.doReqString("api/device/compresslogfile", nil, "LogPath")
}

// LogInfo retrieves current log setting information.
func (c *Client) LogInfo() (XMLData, error) {
	return c.Do("api/device/logsetting", nil)
}

// PhonebookGroupList retrieves list of the phonebook groups.
func (c *Client) PhonebookGroupList(page, count uint, sortByName, ascending bool) (XMLData, error) {
	return c.Do("api/pb/group-list", SimpleRequestXML(
		"PageIndex", fmt.Sprintf("%d", page),
		"ReadCount", fmt.Sprintf("%d", count),
		"SortType", boolToString(sortByName),
		"Ascending", boolToString(ascending),
	))
}

// PhonebookCount retrieves count of phonebook entries per group.
func (c *Client) PhonebookCount() (XMLData, error) {
	return c.Do("api/pb/pb-count", nil)
}

// PhonebookImport imports SIM contacts into specified phonebook group.
func (c *Client) PhonebookImport(group uint) (XMLData, error) {
	return c.Do("api/pb/pb-copySIM", XMLData{
		"GroupID": fmt.Sprintf("%d", group),
	})
}

// PhonebookDelete deletes a specified phonebook entry.
func (c *Client) PhonebookDelete(id uint) (bool, error) {
	return c.doReqCheckOK("api/pb/delete-pb", SimpleRequestXML(
		"Index", fmt.Sprintf("%d", id),
	))
}

// PhonebookList retrieves list of phonebook entries from a specified group.
func (c *Client) PhonebookList(group, page, count uint, sim, sortByName, ascending bool, keyword string) (XMLData, error) {
	// execute request -- note: the order is important!
	return c.Do("api/pb/pb-list", SimpleRequestXML(
		"GroupID", fmt.Sprintf("%d", group),
		"PageIndex", fmt.Sprintf("%d", page),
		"ReadCount", fmt.Sprintf("%d", count),
		"SaveType", boolToString(sim),
		"SortType", boolToString(sortByName),
		"Ascending", boolToString(ascending),
		"KeyWord", keyword,
	))
}

// PhonebookCreate creates a new phonebook entry.
func (c *Client) PhonebookCreate(group uint, name, phone string, sim bool) (XMLData, error) {
	return c.Do("api/pb/pb-new", SimpleRequestXML(
		"GroupID", fmt.Sprintf("%d", group),
		"SaveType", boolToString(sim),
		"Field", xmlNvp("FormattedName", name),
		"Field", xmlNvp("MobilePhone", phone),
		"Field", xmlNvp("HomePhone", ""),
		"Field", xmlNvp("WorkPhone", ""),
		"Field", xmlNvp("WorkEmail", ""),
	))
}

// FirewallFeatures retrieves firewall security feature information.
func (c *Client) FirewallFeatures() (XMLData, error) {
	return c.Do("api/security/firewall-switch", nil)
}

// DmzConfig retrieves DMZ status and IP address of DMZ host.
func (c *Client) DmzConfig() (XMLData, error) {
	return c.Do("api/security/dmz", nil)
}

// DmzConfigSet enables or disables the DMZ and the DMZ IP address of the
// device.
func (c *Client) DmzConfigSet(enabled bool, dmzIPAddress string) (bool, error) {
	return c.doReqCheckOK("api/security/dmz", SimpleRequestXML(
		"DmzIPAddress", dmzIPAddress,
		"DmzStatus", boolToString(enabled),
	))
}

// SipAlg retrieves status and port of the SIP application-level gateway.
func (c *Client) SipAlg() (XMLData, error) {
	return c.Do("api/security/sip", nil)
}

// SipAlgSet enables/disables SIP application-level gateway and sets SIP port.
func (c *Client) SipAlgSet(port uint, enabled bool) (bool, error) {
	return c.doReqCheckOK("api/security/sip", SimpleRequestXML(
		"SipPort", fmt.Sprintf("%d", port),
		"SipStatus", boolToString(enabled),
	))
}

// NatType retrieves NAT type.
func (c *Client) NatType() (XMLData, error) {
	return c.Do("api/security/nat", nil)
}

// NatTypeSet sets NAT type (values: 0, 1).
func (c *Client) NatTypeSet(ntype uint) (bool, error) {
	return c.doReqCheckOK("api/security/nat", SimpleRequestXML(
		"NATType", fmt.Sprintf("%d", ntype),
	))
}

// Upnp retrieves the status of UPNP.
func (c *Client) Upnp() (XMLData, error) {
	return c.Do("api/security/upnp", nil)
}

// UpnpSet enables/disables UPNP.
func (c *Client) UpnpSet(enabled bool) (bool, error) {
	return c.doReqCheckOK("api/security/upnp", SimpleRequestXML(
		"UpnpStatus", boolToString(enabled),
	))
}

// TODO:
// UserLogin/UserLogout/UserPasswordChange
//
// WLAN management
// firewall ("security") configuration
// wifi profile management
