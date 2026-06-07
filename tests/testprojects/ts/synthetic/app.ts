// Synthetic TypeScript test project for MCP-сервер tests
// Tests for: implements, extends, references issues

// Interface definitions
interface IDataProcessor {
    process(data: string): string;
    validate(): boolean;
}

interface IWorker {
    work(): Promise<void>;
    stop(): void;
}

interface IBaseService {
    init(): void;
    getName(): string;
}

// Class implementing interface
class DataProcessor implements IDataProcessor {
    private name: string;

    constructor(name: string) {
        this.name = name;
    }

    process(data: string): string {
        return `processed: ${data}`;
    }

    validate(): boolean {
        return this.name.length > 0;
    }
}

// Class implementing multiple interfaces
class MultiService implements IDataProcessor, IWorker {
    private processor: DataProcessor;
    private active: boolean;

    constructor() {
        this.processor = new DataProcessor("multi");
        this.active = true;
    }

    process(data: string): string {
        return this.processor.process(data);
    }

    validate(): boolean {
        return this.processor.validate();
    }

    async work(): Promise<void> {
        if (!this.active) {
            throw new Error("Not active");
        }
    }

    stop(): void {
        this.active = false;
    }
}

// Class extending another class
class ExtendedProcessor extends DataProcessor {
    private version: number;

    constructor(name: string, version: number) {
        super(name);
        this.version = version;
    }

    process(data: string): string {
        return `v${this.version}: ${super.process(data)}`;
    }
}

// Function using interface
function handleData(processor: IDataProcessor): string {
    if (processor.validate()) {
        return processor.process("task");
    }
    return "invalid";
}

// Function calling methods
function callProcess(processor: IDataProcessor): string {
    return processor.process("test");
}

function callWork(worker: IWorker): Promise<void> {
    return worker.work();
}

// Method references test
class MethodCaller {
    doWork(): string {
        return "working";
    }

    doProcess(data: string): string {
        return this.doWork() + ": " + data;
    }
}

function callMethod(caller: MethodCaller): string {
    return caller.doWork();
}

// Export for testing
export {
    IDataProcessor,
    IWorker,
    IBaseService,
    DataProcessor,
    MultiService,
    ExtendedProcessor,
    handleData,
    callProcess,
    callWork,
    MethodCaller,
    callMethod
};
