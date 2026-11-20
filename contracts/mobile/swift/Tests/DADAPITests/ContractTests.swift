import Foundation
import XCTest
import DADAPI

final class ContractTests: XCTestCase {
    func testNativeSessionRequiresBothCredentials() throws {
        let body = Data(#"{"access_token":"synthetic-access","expires_in":900,"refresh_token":"synthetic-refresh","refresh_expires_in":5184000}"#.utf8)
        let session = try JSONDecoder().decode(Components.Schemas.NativeSession.self, from: body)
        XCTAssertEqual(session.access_token, "synthetic-access")
        XCTAssertEqual(session.refresh_token, "synthetic-refresh")
        let missingRefresh = Data(#"{"access_token":"synthetic-access","expires_in":900,"refresh_expires_in":5184000}"#.utf8)
        XCTAssertThrowsError(try JSONDecoder().decode(Components.Schemas.NativeSession.self, from: missingRefresh))
    }

    private let profileFixture: [String: Any] = [
        "email": "donor@example.org", "public_name": NSNull(), "account_role": "DONOR",
        "zip": NSNull(), "city": NSNull(), "timezone": NSNull(), "phone": NSNull(),
        "referral_emails": true, "marketing_emails": false,
        "push_referral_converted": true, "push_streak_at_risk": true, "push_streak_milestone": true,
        "push_streak_lapsed": true, "push_badge_earned": true, "push_gift_processed": true,
        "push_event_nearby": true, "push_event_reminder": true, "push_event_promoted": true,
        "rankings_opted_out": false, "network_visible": false, "volunteer": false,
        "volunteer_interests": [String](), "version": 3, "consent_version": "synthetic-v1",
    ]

    private func decodeProfile(overriding overrides: [String: Any] = [:]) throws -> Components.Schemas.Profile {
        let body = profileFixture.merging(overrides) { _, new in new }
        return try JSONDecoder().decode(Components.Schemas.Profile.self, from: JSONSerialization.data(withJSONObject: body))
    }

    func testProfileDecodesNullablePublicName() throws {
        let profile = try decodeProfile()
        XCTAssertEqual(profile.account_role, .DONOR)
        XCTAssertNil(profile.public_name)
        XCTAssertNil(profile.zip)
        XCTAssertNil(profile.city)
        XCTAssertNil(profile.timezone)
        XCTAssertNil(profile.phone)
        XCTAssertEqual(profile.version, 3)
        XCTAssertFalse(profile.marketing_emails)
    }

    func testProfileDecodesPopulatedIdentityFields() throws {
        let profile = try decodeProfile(overriding: [
            "public_name": "Dana", "zip": "94103", "city": "San Francisco",
            "timezone": "America/Los_Angeles", "phone": "+14155550123", "version": 1,
        ])
        XCTAssertEqual(profile.public_name, "Dana")
        XCTAssertEqual(profile.zip, "94103")
        XCTAssertEqual(profile.city, "San Francisco")
        XCTAssertEqual(profile.timezone, "America/Los_Angeles")
        XCTAssertEqual(profile.phone, "+14155550123")
    }
}
