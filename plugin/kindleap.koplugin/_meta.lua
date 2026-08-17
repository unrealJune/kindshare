local _ = require("gettext")
return {
    name = "kindleap",
    fullname = _("Kindle SoftAP"),
    description = _([[Turns the Kindle into a Wi-Fi access point with an HTTP file receiver, so a phone can push files to it with no router and no cable.

Control lives on the device on purpose: bringing up the AP severs any remote session used to start it.]]),
}
