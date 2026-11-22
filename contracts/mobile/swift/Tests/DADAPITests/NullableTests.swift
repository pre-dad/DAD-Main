import Foundation
import XCTest
import DADAPI

final class NullableTests: XCTestCase {
    private func decode<T: Decodable>(_ type: T.Type, _ value: [String: Any]) throws -> T {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return try decoder.decode(type, from: JSONSerialization.data(withJSONObject: value))
    }

    func testDashboardRetainsUnknownAndPopulatedFinancialStates() throws {
        var body: [String: Any] = [
            "mode": "test", "state": "ready", "computed_at": "2026-10-26T12:00:00Z", "founding_donor": false,
            "referrals": NSNull(), "streak": NSNull(), "rank": NSNull(), "rankings_opted_out": false, "network": NSNull(),
            "impact": ["given_minor": NSNull(), "currency": "USD", "initiatives_funded": 0, "events_volunteered": 0],
        ]
        // The generator numbers oneOf variants in spec order (case1 is the preparing
        // state, case2 the ready one); a reordered schema fails here with the variant named.
        guard case .case2(let unknown) = try decode(Components.Schemas.DonorDashboard.self, body) else {
            return XCTFail("ready dashboard lost its response variant")
        }
        XCTAssertNil(unknown.referrals)
        XCTAssertNil(unknown.streak)
        XCTAssertNil(unknown.rank)
        XCTAssertFalse(unknown.rankings_opted_out)
        XCTAssertNil(unknown.network)
        XCTAssertNil(unknown.impact.given_minor)
        body["rankings_opted_out"] = true
        guard case .case2(let optedOut) = try decode(Components.Schemas.DonorDashboard.self, body) else {
            return XCTFail("opted-out dashboard lost its response variant")
        }
        XCTAssertTrue(optedOut.rankings_opted_out)
        body["rankings_opted_out"] = false
        body["streak"] = ["at_risk": true, "current": 30, "grace_ends_at": NSNull(), "ladder": NSNull()]
        body["rank"] = ["national_percentile": 2, "city": "Boston", "city_rank": 4, "computed_at": "2026-10-26T12:00:00Z"]
        body["network"] = ["active_count": 3, "annualized_minor": 108000, "currency": "USD"]
        body["impact"] = ["given_minor": 9000, "currency": "USD", "initiatives_funded": 2, "events_volunteered": 1]
        guard case .case2(let known) = try decode(Components.Schemas.DonorDashboard.self, body) else {
            return XCTFail("populated dashboard lost its response variant")
        }
        XCTAssertEqual(known.streak?.current, 30)
        XCTAssertNil(known.streak?.ladder)
        XCTAssertEqual(known.rank?.national_percentile, 2)
        XCTAssertEqual(known.network?.annualized_minor, 108000)
        XCTAssertEqual(known.impact.given_minor, 9000)
        body["streak"] = ["current": "not a count"]
        XCTAssertThrowsError(try decode(Components.Schemas.DonorDashboard.self, body))
    }

    func testRecurringSubscriptionDecodesNullAndCurrentState() throws {
        var body: [String: Any] = ["mode": "test", "state": "ready", "subscription": NSNull()]
        XCTAssertNil(try decode(Components.Schemas.RecurringDetails.self, body).subscription)
        body["subscription"] = [
            "amount_minor": 3000, "currency": "USD", "id": "00000000-0000-4000-8000-000000000001",
            "period_end": "2026-11-26T12:00:00Z", "status": "active", "stops_at": NSNull(),
        ]
        let current = try decode(Components.Schemas.RecurringDetails.self, body)
        XCTAssertEqual(current.subscription?.amount_minor, 3000)
        XCTAssertNil(current.subscription?.stops_at)
        body["subscription"] = ["amount_minor": "invalid"]
        XCTAssertThrowsError(try decode(Components.Schemas.RecurringDetails.self, body))
    }

    func testEventNullableFieldsAndReservationEnum() throws {
        var body: [String: Any] = [
            "id": "synthetic-event", "slug": "community-day", "title": "Community day", "type": "VOLUNTEER",
            "description": "Help at the event", "venue": "Community centre", "address": "1 Test Street", "city": "Boston",
            "starts_at": "2026-11-26T12:00:00Z", "ends_at": "2026-11-26T14:00:00Z", "capacity": 0,
            "spots_left": NSNull(), "reservation_status": NSNull(), "invitation_note": NSNull(),
            "report_issue_url": NSNull(), "cancelled": false, "booking_opens_at": NSNull(), "early_booking": false,
            "time_zone": "America/New_York", "checked_in_at": NSNull(),
        ]
        let open = try decode(Components.Schemas.EventView.self, body)
        XCTAssertNil(open.spots_left)
        XCTAssertNil(open.reservation_status)
        XCTAssertNil(open.booking_opens_at)
        XCTAssertNil(open.checked_in_at)
        XCTAssertEqual(open.time_zone, "America/New_York")
        body["spots_left"] = 5
        body["reservation_status"] = "RESERVED"
        body["booking_opens_at"] = "2026-11-01T12:00:00Z"
        body["checked_in_at"] = "2026-11-26T12:05:00Z"
        let reserved = try decode(Components.Schemas.EventView.self, body)
        XCTAssertEqual(reserved.spots_left, 5)
        XCTAssertEqual(reserved.reservation_status?.rawValue, "RESERVED")
        XCTAssertNotNil(reserved.booking_opens_at)
        XCTAssertNotNil(reserved.checked_in_at)
        body["reservation_status"] = "INVENTED"
        XCTAssertThrowsError(try decode(Components.Schemas.EventView.self, body))
    }

    func testCompletedPaymentUpdateRequiresALiteralNullURL() throws {
        var body: [String: Any] = ["completed_at": NSNull(), "status": "APPLIED", "url": NSNull()]
        let completed = try decode(Components.Schemas.PaymentUpdate.self, body)
        let encoded = try JSONSerialization.jsonObject(with: JSONEncoder().encode(completed)) as? [String: Any]
        XCTAssertTrue(encoded?["url"] is NSNull)
        body["url"] = "https://unexpected.test"
        XCTAssertThrowsError(try decode(Components.Schemas.PaymentUpdate.self, body))
        XCTAssertThrowsError(try JSONDecoder().decode(DADNullValue.self, from: Data("false".utf8)))
    }
}
