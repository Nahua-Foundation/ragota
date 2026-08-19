namespace Acme.Notifier;

// NotificationStore persists notifications.
public class NotificationStore
{
    // Save stores a pending notification.
    public void Save(string userId, string invoiceId)
    {
        // omitted: database write
    }

    // FindByUser returns notifications for a user.
    public object FindByUser(string userId)
    {
        return new object[] { };
    }
}
