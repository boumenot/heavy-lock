import Foundation

enum StatusReader {
    static func read(directory: URL) -> (records: [StatusRecord], readError: Bool) {
        let files: [URL]
        do {
            files = try FileManager.default.contentsOfDirectory(
                at: directory, includingPropertiesForKeys: nil
            )
        } catch {
            let error = error as NSError
            if error.domain == NSCocoaErrorDomain &&
                (error.code == NSFileReadNoSuchFileError || error.code == NSFileNoSuchFileError) {
                return ([], false)
            }
            return ([], true)
        }
        var records: [StatusRecord] = []
        var readError = false
        for file in files where file.pathExtension == "json" {
            do {
                records.append(try JSONDecoder().decode(StatusRecord.self, from: Data(contentsOf: file)))
            } catch {
                let error = error as NSError
                if error.domain == NSCocoaErrorDomain &&
                    (error.code == NSFileReadNoSuchFileError || error.code == NSFileNoSuchFileError) {
                    continue
                }
                readError = true
            }
        }
        return (records, readError)
    }
}
