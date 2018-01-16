package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func connectToPeer(t *Transfer) bool {

	if t.Mode == "receiving" {
		if !addFirewallRule(t) {
			return false
		}
		if !startAdHoc(t) {
			return false
		}
	} else if t.Mode == "sending" {
		if !checkForFile(t) {
			t.output(fmt.Sprintf("Could not find file to send: %s", t.Filepath))
			return false
		}
		if t.Peer == "windows" {
			if !joinAdHoc(t) {
				return false
			}
			t.RecipientIP = findPeer(t)
		} else if t.Peer == "mac" || t.Peer == "linux" {
			if !addFirewallRule(t) {
				return false
			}
			if !startAdHoc(t) {
				return false
			}
			t.RecipientIP = findPeer(t)
		}
	}
	return true
}

func startAdHoc(t *Transfer) bool {

	runCommand("netsh winsock reset")
	runCommand("netsh wlan stop hostednetwork")
	t.output("SSID: " + t.SSID)
	runCommand("netsh wlan set hostednetwork mode=allow ssid=" + t.SSID + " key=" + t.Passphrase + t.Passphrase)
	cmd := exec.Command("netsh", "wlan", "start", "hostednetwork")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_, err := cmd.CombinedOutput()
	// TODO: replace with "echo %errorlevel%" == "1"
	if err.Error() == "exit status 1" {
		t.output("Could not start hosted network, trying Wi-Fi Direct.")
		t.AdHocCapable = false

		startChan := make(chan bool)
		go startLegacyAP(t, startChan)
		if ok := <-startChan; !ok {
			return false
		}
		return true
	} else if err == nil {
		t.AdHocCapable = true
		return true
	} else {
		t.output(fmt.Sprintf("Could not start hosted network."))
		resetWifi(t)
		return false
	}
}

func stopAdHoc(t *Transfer) {
	if t.AdHocCapable {
		t.output(runCommand("netsh wlan stop hostednetwork"))
	} else {
		t.output("Stopping Wi-Fi Direct.")
		// TODO: blocking operation, check wifiDirect function is running.
		t.WifiDirectChan <- "quit"
		reply := <-t.WifiDirectChan
		t.output(reply)
		close(t.WifiDirectChan)
	}
}

func joinAdHoc(t *Transfer) bool {
	cmd := exec.Command("cmd", "/C", "echo %USERPROFILE%")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmdBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.output("Error getting temp location.")
		return false
	}
	tmpLoc := strings.TrimSpace(string(cmdBytes)) + "\\AppData\\Local\\Temp\\adhoc.xml"

	// make doc
	xmlDoc := "<?xml version=\"1.0\"?>\r\n" +
		"<WLANProfile xmlns=\"http://www.microsoft.com/networking/WLAN/profile/v1\">\r\n" +
		"	<name>" + t.SSID + "</name>\r\n" +
		"	<SSIDConfig>\r\n" +
		"		<SSID>\r\n" +
		"			<name>" + t.SSID + "</name>\r\n" +
		"		</SSID>\r\n" +
		"	</SSIDConfig>\r\n" +
		"	<connectionType>ESS</connectionType>\r\n" +
		"	<connectionMode>auto</connectionMode>\r\n" +
		"	<MSM>\r\n" +
		"		<security>\r\n" +
		"			<authEncryption>\r\n" +
		"				<authentication>WPA2PSK</authentication>\r\n" +
		"				<encryption>AES</encryption>\r\n" +
		"				<useOneX>false</useOneX>\r\n" +
		"			</authEncryption>\r\n" +
		"			<sharedKey>\r\n" +
		"				<keyType>passPhrase</keyType>\r\n" +
		"				<protected>false</protected>\r\n" +
		"				<keyMaterial>" + t.Passphrase + t.Passphrase + "</keyMaterial>\r\n" +
		"			</sharedKey>\r\n" +
		"		</security>\r\n" +
		"	</MSM>\r\n" +
		"	<MacRandomization xmlns=\"http://www.microsoft.com/networking/WLAN/profile/v3\">\r\n" +
		"		<enableRandomization>false</enableRandomization>\r\n" +
		"	</MacRandomization>\r\n" +
		"</WLANProfile>"
	// delete file if there
	os.Remove(tmpLoc)

	// write file
	outFile, err := os.OpenFile(tmpLoc, os.O_CREATE|os.O_RDWR, 0744)
	if err != nil {
		resetWifi(t)
		t.output("Write error")
		return false
	}
	data := []byte(xmlDoc)
	if _, err = outFile.Write(data); err != nil {
		resetWifi(t)
		t.output("Write error")
		return false
	}
	defer os.Remove(tmpLoc)

	// add profile
	t.output(runCommand("netsh wlan add profile filename=" + tmpLoc + " user=current"))

	// join network
	t.output("Looking for ad-hoc network " + t.SSID + " for " + strconv.Itoa(JOIN_ADHOC_TIMEOUT) + " seconds...")
	timeout := JOIN_ADHOC_TIMEOUT
	for t.SSID != getCurrentWifi(t) {
		if timeout <= 0 {
			t.output("Could not find the ad hoc network within " + strconv.Itoa(JOIN_ADHOC_TIMEOUT) + " seconds.")
			return false
		}
		cmdStr := "netsh wlan connect name=" + t.SSID
		cmdSlice := strings.Split(cmdStr, " ")
		joinCmd := exec.Command(cmdSlice[0], cmdSlice[1:]...)
		joinCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		/*_, cmdErr :=*/ joinCmd.CombinedOutput()
		// if cmdErr != nil {
		// 	t.output(fmt.Sprintf("Failed to find the ad hoc network. Trying for %2d more seconds. %s", timeout, cmdErr))
		// }
		timeout -= 3
		time.Sleep(time.Second * time.Duration(3))
	}
	return true
}

func findPeer(t *Transfer) (peerIP string) {

	ipPattern, _ := regexp.Compile("\\d{1,3}\\.\\d{1,3}\\.\\d{1,3}\\.\\d{1,3}")

	// clear arp cache
	runCommand("arp -d *")

	// get ad hoc ip
	var ifAddr string
	for !ipPattern.Match([]byte(ifAddr)) {
		ifString := "$(ipconfig | Select-String -Pattern '(?<ipaddr>192\\.168\\.(137|173)\\..*)').Matches.Groups[2].Value.Trim()"
		ifCmd := exec.Command("powershell", "-c", ifString)
		ifCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		ifBytes, err := ifCmd.CombinedOutput()
		if err != nil {
			t.output("Error getting ad hoc IP, retrying.")
		}
		ifAddr = strings.TrimSpace(string(ifBytes))
		time.Sleep(time.Second * time.Duration(2))
	}

	// necessary for wifi direct ip addresses
	var thirdOctet string
	if strings.Contains(ifAddr, "137") {
		thirdOctet = "137"
	} else {
		thirdOctet = "173"
	}

	// run arp for that ip
	for !ipPattern.Match([]byte(peerIP)) {
		peerString := "$(arp -a -N " + ifAddr + " | Select-String -Pattern '(?<ip>192\\.168\\." + thirdOctet + "\\.\\d{1,3})' | Select-String -NotMatch '(?<nm>(" + ifAddr + "|192.168." + thirdOctet + ".255)\\s)').Matches.Value"
		peerCmd := exec.Command("powershell", "-c", peerString)
		peerCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		peerBytes, err := peerCmd.CombinedOutput()
		if err != nil {
			t.output("Error getting peer IP, retrying.")
		}
		peerIP = strings.TrimSpace(string(peerBytes))
		time.Sleep(time.Second * time.Duration(2))
	}
	t.output(fmt.Sprintf("Peer IP found: %s", peerIP))
	return
}

func getCurrentWifi(t *Transfer) (SSID string) {
	cmdStr := "$(netsh wlan show interfaces | Select-String -Pattern 'Profile *: (?<profile>.*)').Matches.Groups[1].Value.Trim()"
	cmd := exec.Command("powershell", "-c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmdBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.output("Error getting current SSID.")
	}
	SSID = strings.TrimSpace(string(cmdBytes))
	return
}

func getWifiInterface() string {
	return ""
}

func resetWifi(t *Transfer) {
	if t.Mode == "receiving" || t.Peer == "mac" || t.Peer == "linux" {
		deleteFirewallRule(t)
		stopAdHoc(t)
	} else { // if Mode == "sending" && t.Peer == "windows"
		runCommand("netsh wlan delete profile name=" + t.SSID)
		// rejoin previous wifi
		t.output(runCommand("netsh wlan connect name=" + t.PreviousSSID))
	}
}

func addFirewallRule(t *Transfer) bool {

	execPath, err := os.Executable()
	if err != nil {
		t.output("Failed to get executable path.")
		return false
	}
	cmd := exec.Command("netsh", "advfirewall", "firewall", "add", "rule", "name=flyingcarpet", "dir=in",
		"action=allow", "program='"+execPath+"'", "enable=yes", "profile=any", "localport=3290", "protocol=tcp")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_, err = cmd.CombinedOutput()
	if err != nil {
		t.output("Could not create firewall rule. You must run as administrator to receive. (Press Win+X and then A to start an administrator command prompt.)")
		t.output(err.Error())
		return false
	}
	// t.output("Firewall rule created.")
	return true
}

func deleteFirewallRule(t *Transfer) {
	fwStr := "netsh advfirewall firewall delete rule name=flyingcarpet"
	t.output(runCommand(fwStr))
}

func checkForFile(t *Transfer) bool {
	_, err := os.Stat(t.Filepath)
	if err != nil {
		return false
	}
	return true
}

func runCommand(cmdStr string) (output string) {
	var cmdBytes []byte
	err := errors.New("")
	cmdSlice := strings.Split(cmdStr, " ")
	if len(cmdSlice) > 1 {
		cmd := exec.Command(cmdSlice[0], cmdSlice[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		cmdBytes, err = cmd.CombinedOutput()
	} else {
		cmd := exec.Command(cmdStr)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		cmdBytes, err = cmd.CombinedOutput()
	}
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(string(cmdBytes))
}

func getCurrentUUID(t *Transfer) (uuid string) { return "" }
