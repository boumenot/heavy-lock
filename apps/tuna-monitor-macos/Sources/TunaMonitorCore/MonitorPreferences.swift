import Foundation

public enum MonitorPreferences {
    public static func registerDefaults(in defaults: UserDefaults = .standard) {
        defaults.register(defaults: ["NSInitialToolTipDelay": 1_000])
    }
}
