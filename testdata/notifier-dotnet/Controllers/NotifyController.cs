using Microsoft.AspNetCore.Mvc;

namespace Acme.Notifier.Controllers;

// NotifyController accepts notification requests from other services.
[ApiController]
[Route("api/notify")]
public class NotifyController : ControllerBase
{
    private readonly NotificationStore _store;

    public NotifyController(NotificationStore store)
    {
        _store = store;
    }

    // Send delivers a notification to the user about an invoice.
    [HttpPost("send")]
    public IActionResult Send([FromBody] NotifyRequest req)
    {
        SaveNotification(req.UserId, req.InvoiceId);
        return Ok();
    }

    // ListNotifications returns notifications for a user.
    [HttpGet("list/{userId}")]
    public IActionResult ListNotifications(string userId)
    {
        return Ok(_store.FindByUser(userId));
    }

    // SaveNotification persists the notification for delivery.
    private void SaveNotification(string userId, string invoiceId)
    {
        _store.Save(userId, invoiceId);
    }
}

// NotifyRequest is the incoming notification payload.
public class NotifyRequest
{
    public string UserId { get; set; } = "";
    public string InvoiceId { get; set; } = "";
}
