import com.example.api.Service;
import com.example.impl.ServiceImpl;
public class Main {
    public static void main(String[] args) {
        Service s = new ServiceImpl();
        s.execute();
    }
}
