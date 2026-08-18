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
local InputDialog = require("ui/widget/inputdialog")
local TextViewer = require("ui/widget/textviewer")
local UIManager = require("ui/uimanager")
local WidgetContainer = require("ui/widget/container/widgetcontainer")
local _ = require("gettext")
local T = require("ffi/util").template
local logger = require("logger")

local BASE = "/mnt/us/kindler"
local AP = BASE .. "/kindle-ap.sh"
local SVC = BASE .. "/kindshare-svc.sh"
local AUTOSTART = BASE .. "/autostart"
local IDENTITY = BASE .. "/identity"
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

-- ------------------------------------------------------------------ identity
--
-- The daemon owns this file and creates it on first start; we only ever edit
-- the name and clear the id. devid in particular must survive untouched, so
-- every write passes unknown lines through rather than regenerating the file.

local function readIdentity()
    local id = { name = nil, id = nil }
    local f = io.open(IDENTITY, "r")
    if not f then return id end
    for line in f:lines() do
        local k, v = line:match("^%s*([%w_]+)%s*=%s*(.-)%s*$")
        if k == "name" then id.name = v
        elseif k == "id" then id.id = v end
    end
    f:close()
    return id
end

-- displayName is what the phone shows: the name with the id appended. Kept
-- identical to identity.Display() in the Go side.
local function displayName()
    local id = readIdentity()
    if not id.name then return nil end
    if id.id and id.id ~= "" then return id.name .. " " .. id.id end
    return id.name
end

-- writeIdentityField rewrites one key in place, preserving every other line.
-- Written to a temp file and renamed so the daemon never reads a half-written
-- one. A nil value deletes the key, which is how the id gets regenerated: the
-- daemon fills in anything missing on next start.
local function writeIdentityField(key, value)
    local lines, seen = {}, false
    local f = io.open(IDENTITY, "r")
    if f then
        for line in f:lines() do
            local k = line:match("^%s*([%w_]+)%s*=")
            if k == key then
                seen = true
                if value then table.insert(lines, key .. "=" .. value) end
            else
                table.insert(lines, line)
            end
        end
        f:close()
    end
    if not seen and value then table.insert(lines, key .. "=" .. value) end

    local tmp = IDENTITY .. ".tmp"
    local out = io.open(tmp, "w")
    if not out then return false, "cannot write " .. tmp end
    out:write(table.concat(lines, "\n") .. "\n")
    out:close()
    -- rename over the existing file, not remove-then-rename: on POSIX this is
    -- atomic, and the daemon may be reading it at any moment.
    local ok = os.rename(tmp, IDENTITY)
    if not ok then
        os.remove(tmp)
        return false, "cannot replace " .. IDENTITY
    end
    sh("sync")
    return true
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

-- ------------------------------------------------------------ identity edits

-- applyIdentity writes the change and restarts the daemon if it is running.
-- The identity is read once at startup, so an edit means nothing until the
-- process comes back - doing it silently here avoids a setting that appears to
-- have had no effect.
function KindleAP:applyIdentity(touchmenu_instance, ok, err)
    if not ok then
        self:toast(T(_("Could not save: %1"), err or _("unknown error")))
        return
    end
    if serviceRunning() then
        sh(SVC .. " restart")
        self:toast(T(_("Now shows as: %1"), displayName() or "?"))
    else
        self:toast(_("Saved. Takes effect when receiving starts."))
    end
    if touchmenu_instance then touchmenu_instance:updateItems() end
end

function KindleAP:editDeviceName(touchmenu_instance)
    local current = readIdentity()
    if not current.name then
        self:toast(_("Start receiving once first - the daemon creates the identity file."))
        return
    end

    local dialog
    dialog = InputDialog:new{
        title = _("Device name"),
        input = current.name,
        description = _([[The name your phone shows in the Quick Share sheet.

The four-character ID is always appended, so two Kindles can still be told apart.]]),
        buttons = {{
            {
                text = _("Cancel"),
                id = "close",
                callback = function() UIManager:close(dialog) end,
            },
            {
                text = _("Save"),
                is_enter_default = true,
                callback = function()
                    -- One line, no leading or trailing space: the value goes
                    -- into a key=value file and into a DNS label.
                    local name = (dialog:getInputText() or "")
                        :gsub("[\r\n]", " "):gsub("^%s+", ""):gsub("%s+$", "")
                    UIManager:close(dialog)
                    if name == "" then
                        self:toast(_("Name cannot be empty."))
                        return
                    end
                    local ok, err = writeIdentityField("name", name)
                    self:applyIdentity(touchmenu_instance, ok, err)
                end,
            },
        }},
    }
    UIManager:show(dialog)
    dialog:onShowKeyboard()
end

function KindleAP:regenerateDeviceID(touchmenu_instance)
    local current = readIdentity()
    if not current.name then
        self:toast(_("Start receiving once first - the daemon creates the identity file."))
        return
    end

    UIManager:show(ConfirmBox:new{
        text = T(_([[Give this device a new ID?

It currently shows as "%1".

The ID is what keeps your phone recognising this Kindle between restarts. Changing it makes the phone treat this as a brand-new device, and the old entry may linger in the share sheet for a while.

Only worth doing if two devices ended up with the same ID.]]), displayName() or "?"),
        ok_text = _("New ID"),
        ok_callback = function()
            -- Deleting the key is the whole mechanism: the daemon generates and
            -- persists anything missing at startup, so there is no second
            -- generator here to disagree with it.
            local ok, err = writeIdentityField("id", nil)
            self:applyIdentity(touchmenu_instance, ok, err)
        end,
    })
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
                callback = function() self:run(SVC .. " status", _("Quick Share status")) end,
            },
            {
                text_func = function()
                    local shown = displayName()
                    if not shown then
                        return _("Device name: (set on first start)")
                    end
                    return T(_("Device name: %1"), shown)
                end,
                keep_menu_open = true,
                callback = function(touchmenu_instance)
                    self:editDeviceName(touchmenu_instance)
                end,
            },
            {
                text = _("Change device ID"),
                keep_menu_open = true,
                separator = true,
                callback = function(touchmenu_instance)
                    self:regenerateDeviceID(touchmenu_instance)
                end,
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
