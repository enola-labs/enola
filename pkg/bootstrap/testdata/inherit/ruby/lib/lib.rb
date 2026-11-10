class Repo
  def save; puts "save"; end
end
class Helper
  def work; puts "work"; end
end
class Base
  def initialize
    @helper = Helper.new
  end
  def run
    step
  end
  def common
    @helper.work
  end
  def step
    raise NotImplementedError
  end
end
