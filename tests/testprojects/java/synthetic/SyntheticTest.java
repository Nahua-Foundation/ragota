// Synthetic Java test project for MCP-сервер tests
// Tests for: implements, extends, references issues

package synthetic;

// Interface definitions
interface IProcessor {
    String process(String data);
    boolean validate();
}

interface IWorker {
    void work() throws Exception;
    void stop();
}

interface IService {
    void init();
    String getName();
}

// Class implementing interface
class DataProcessor implements IProcessor {
    private String name;

    public DataProcessor(String name) {
        this.name = name;
    }

    public String process(String data) {
        return "processed: " + data;
    }

    public boolean validate() {
        return name != null && !name.isEmpty();
    }
}

// Class implementing multiple interfaces
class MultiService implements IProcessor, IWorker {
    private DataProcessor processor;
    private boolean active;

    public MultiService() {
        this.processor = new DataProcessor("multi");
        this.active = true;
    }

    public String process(String data) {
        return processor.process(data);
    }

    public boolean validate() {
        return processor.validate();
    }

    public void work() throws Exception {
        if (!active) {
            throw new Exception("Not active");
        }
    }

    public void stop() {
        active = false;
    }
}

// Class extending another class
class ExtendedProcessor extends DataProcessor {
    private int version;

    public ExtendedProcessor(String name, int version) {
        super(name);
        this.version = version;
    }

    public String process(String data) {
        return "v" + version + ": " + super.process(data);
    }
}

// Abstract class for testing
abstract class BaseService implements IService {
    protected String name;

    public BaseService(String name) {
        this.name = name;
    }

    public String getName() {
        return name;
    }

    public abstract void execute();
}

// Concrete implementation of abstract class
class ConcreteService extends BaseService {
    public ConcreteService(String name) {
        super(name);
    }

    public void init() {
        System.out.println("Initializing: " + getName());
    }

    public void execute() {
        System.out.println("Executing: " + getName());
    }
}

// Function using interface
class ServiceUtil {
    public static String handleData(IProcessor processor) {
        if (processor.validate()) {
            return processor.process("task");
        }
        return "invalid";
    }

    public static String callProcess(IProcessor processor) {
        return processor.process("test");
    }

    public static void callWorker(IWorker worker) throws Exception {
        worker.work();
    }
}

// Method references test
class MethodCaller {
    public String doWork() {
        return "working";
    }

    public String doProcess(String data) {
        return doWork() + ": " + data;
    }
}

class MethodCallerTest {
    public static String callMethod(MethodCaller caller) {
        return caller.doWork();
    }
}

// Main class for testing
public class SyntheticTest {
    public static void main(String[] args) {
        DataProcessor dp = new DataProcessor("test");
        MultiService ms = new MultiService();
        ExtendedProcessor ep = new ExtendedProcessor("ext", 2);
        ConcreteService cs = new ConcreteService("concrete");

        ServiceUtil.handleData(dp);
        ServiceUtil.callProcess(dp);

        MethodCaller mc = new MethodCaller();
        MethodCallerTest.callMethod(mc);
        mc.doProcess("data");

        System.out.println("Synthetic Java test project");
    }
}
