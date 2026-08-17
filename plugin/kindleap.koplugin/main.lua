--[[
Kindle SoftAP + Quick Share - on-device control.

Two separate things, deliberately:

  * kindshare   an always-on daemon that receives Quick Share transfers. It is
                NOT run from KOReader - KOReader is not always open, and
                "receiving whenever wifi is up" cannot depend on a reader app.
                This plugin only starts, stops and inspects it.

  * SoftAP      turns wlan0 into an access point for when there is no network
                to share. The daemon needs no special handling for this: the AP
                gives wlan0 a new address, the daemon notices and re-advertises
                there by itself.

Control lives on the device because bringing up the AP severs whatever remote
session was used to start it.
--]]

local ConfirmBox = require("ui/widget/confirmbox")
local Font = require("ui/font")
local InfoMessage = require("ui/widget/infomessage")
local TextViewer = require("ui/widget/textviewer")
local UIManager = require("ui/uimanager")
local WidgetContainer = require("ui/widget/container/widgetcontainer")
local _ = require("gettext")
local logger = require("logger")

local BASE = "/mnt/us/kindler"
local AP = BASE .. "/kindle-ap.sh"
local SVC = BASE .. "/kindshare-svc.sh"
local AUTOSTART = BASE .. "/autostart"
local LOGDIR = BASE .. "/logs"

local AP_LOG = "/tmp/kindle-ap.log"
local HOSTAPD_LOG = "/tmp/hostapd.log"
local UDHCPD_LOG = "/tmp/udhcpd.log"
local KINDROP_LOG = "/tmp/kindrop.log"
local SHARE_LOG = "/tmp/kindshare.log"
local STATUS = "/tmp/kindshare-status.json"

local KindleAP = WidgetContainer:extend{
    name = "kindleap",
    is_doc_only = false,
}

local function sh(cmd)
    local p = io.popen(cmd .. " 2>&1", "r")
    if not p then return "(could not run: " .. cmd .. ")" end
    local out = p:read("*a") or ""
    p:close()
    return out
end

local function fileExists(path)
    local f = io.open(path, "r")
    if f then f:close() return true end
    return false
end

local function readFile(path, tail_lines)
    if not fileExists(path) then return "(missing: " .. path .. ")" end
    if tail_lines then return sh("tail -n " .. tail_lines .. " " .. path) end
    return sh("cat " .. path)
end

local function serviceRunning()
    return sh("pidof kindshare"):match("%d") ~= nil
end

function KindleAP:init()
    self.ui.menu:registerToMainMenu(self)
end

-- ------------------------------------------------------------------ helpers

function KindleAP:run(cmd, title)
    local info = InfoMessage:new{ text = _("Working…") }
    UIManager:show(info)
    UIManager:nextTick(function()
        local out = sh(cmd)
        UIManager:close(info)
        logger.info("kindleap: " .. cmd .. " ->\n" .. out)
        UIManager:show(TextViewer:new{
            title = title,
            text = out ~= "" and out or _("(no output)"),
            text_face = Font:getFace("smallinfont"),
            justified = false,
        })
    end)
end

function KindleAP:toast(text)
    UIManager:show(InfoMessage:new{ text = text, timeout = 4 })
end

-- ---------------------------------------------------------------- diagnostics

function KindleAP:collect()
    local parts = {}
    local function section(title, body)
        table.insert(parts, "===== " .. title .. " =====\n" .. (body or "") .. "\n")
    end

    section("date / uptime", sh("date; uptime"))
    section("service", sh(SVC .. " status"))
    section("wlan0 (nl80211)", sh("iw dev wlan0 info; iw dev wlan0 link"))
    section("interfaces", sh("ifconfig wlan0 2>&1 | head -4"))
    section("processes", sh("ps | grep -E '[h]ostapd|[u]dhcpd|[k]indrop|[k]indshare|[w]ifid|[w]pa_supplicant'"))
    section("iptables INPUT (with counters)", sh("iptables -L INPUT -n -v --line-numbers | head -30"))
    section("kindshare log", readFile(SHARE_LOG, 60))
    section("ap script log", readFile(AP_LOG, 80))
    section("timeline (last samples)", readFile("/tmp/ap-samples.log", 60))
    section("hostapd: station events", sh(
        "grep -aE 'AP-STA|authorized|assoc|deauth' " .. HOSTAPD_LOG .. " 2>/dev/null | tail -30"))
    section("udhcpd (did any DISCOVER arrive?)", readFile(UDHCPD_LOG, 30))
    section("kindrop log", readFile(KINDROP_LOG, 20))
    section("dmesg (wifi)", sh("dmesg | grep -iE 'ath6kl|wlan|hostapd' | tail -25"))

    return table.concat(parts, "\n")
end

function KindleAP:saveDiagnostics()
    os.execute("mkdir -p " .. LOGDIR)
    local stamp = os.date("%Y%m%d-%H%M%S")
    local path = LOGDIR .. "/diag-" .. stamp .. ".txt"
    local f = io.open(path, "w")
    if not f then
        self:toast(_("Could not write diagnostics."))
        return
    end
    f:write(self:collect())
    f:close()
    -- The raw logs too: collect() greps them down for readability, but the full
    -- output is what actually diagnoses a failure, and /tmp does not survive a
    -- reboot.
    for name, src in pairs({
        kindshare = SHARE_LOG, hostapd = HOSTAPD_LOG, udhcpd = UDHCPD_LOG,
        apscript = AP_LOG, kindrop = KINDROP_LOG, samples = "/tmp/ap-samples.log",
    }) do
        os.execute("cp " .. src .. " " .. LOGDIR .. "/" .. name .. "-" .. stamp .. ".log 2>/dev/null")
    end
    os.execute("sync")
    self:toast(_("Saved:\n") .. path)
end

-- ---------------------------------------------------------------------- menu

function KindleAP:addToMainMenu(menu_items)
    menu_items.kindleap = {
        text = _("Quick Share / SoftAP"),
        sorting_hint = "network",
        sub_item_table = {
            {
                text_func = function()
                    return serviceRunning()
                        and _("Receiving: ON  (tap to stop)")
                        or  _("Receiving: off (tap to start)")
                end,
                keep_menu_open = true,
                callback = function()
                    if serviceRunning() then
                        self:run(SVC .. " stop", _("Quick Share stopped"))
                    else
                        self:run(SVC .. " start", _("Quick Share started"))
                    end
                end,
            },
            {
                text = _("Start receiving at boot"),
                checked_func = function() return fileExists(AUTOSTART) end,
                keep_menu_open = true,
                callback = function()
                    if fileExists(AUTOSTART) then
                        sh(SVC .. " disable")
                        self:toast(_("Will not start at boot."))
                    else
                        sh(SVC .. " enable")
                        self:toast(_("Will start at boot."))
                    end
                end,
            },
            {
                text = _("Service status"),
                keep_menu_open = true,
                separator = true,
                callback = function() self:run(SVC .. " status", _("Quick Share status")) end,
            },
            {
                text = _("Start access point"),
                keep_menu_open = true,
                callback = function()
                    UIManager:show(ConfirmBox:new{
                        text = _([[Start the access point?

wlan0 leaves station mode, so normal Wi-Fi (and any SSH session) drops. Quick Share keeps working - the receiver follows the new address automatically.

The AP reverts on its own if nothing connects.]]),
                        ok_text = _("Start"),
                        ok_callback = function()
                            self:run(AP .. " up", _("SoftAP started"))
                        end,
                    })
                end,
            },
            {
                text = _("Stop access point, restore Wi-Fi"),
                keep_menu_open = true,
                callback = function() self:run(AP .. " down", _("SoftAP stopped")) end,
            },
            {
                text = _("Keep access point up (disarm timer)"),
                keep_menu_open = true,
                separator = true,
                callback = function() self:run(AP .. " commit", _("Timer disarmed")) end,
            },
            {
                text = _("Diagnostics"),
                keep_menu_open = true,
                callback = function()
                    UIManager:show(TextViewer:new{
                        title = _("Diagnostics"),
                        text = self:collect(),
                        text_face = Font:getFace("smallinfont"),
                        justified = false,
                    })
                end,
            },
            {
                text = _("Save diagnostics to /mnt/us"),
                keep_menu_open = true,
                callback = function() self:saveDiagnostics() end,
            },
        },
    }
end

return KindleAP
